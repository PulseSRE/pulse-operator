package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// The Temporal sub-reconciler provisions a server for durable plan execution.
// The properties pinned here are the ones the agent's behaviour depends on:
// the Service name the agent dials, the PostgreSQL reuse (same service, same
// auth secret the rest of the operator manages), and the pinned image — an
// unpinned server image would run schema migrations on a reschedule at a
// moment nobody chose.
var _ = Describe("TemporalReconciler", func() {
	const namespace = "default"

	var (
		ctx context.Context
		tr  *TemporalReconciler
	)

	enabled := true

	BeforeEach(func() {
		ctx = testCtx
		tr = &TemporalReconciler{Client: k8sClient, Scheme: testScheme}
	})

	Describe("disabled (the default)", func() {
		It("creates nothing", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: "temporal-off-pulse", Namespace: namespace},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			Expect(tr.reconcileTemporal(ctx, cr)).To(Succeed())

			deploy := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "temporal-off-pulse-temporal", Namespace: namespace}, deploy)
			Expect(err).To(HaveOccurred(), "no Deployment must exist when temporal is disabled")
		})
	})

	Describe("enabled", func() {
		const crName = "temporal-on-pulse"

		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Temporal: pulsev1alpha1.TemporalConfig{Enabled: &enabled},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
			Expect(tr.reconcileTemporal(ctx, cr)).To(Succeed())
		})

		It("creates the Deployment against the operator's own PostgreSQL", func() {
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-temporal", Namespace: namespace}, deploy)).To(Succeed())

			// OpenShift runs pods as an arbitrary UID; the image's config dir is
			// UID-1000-owned. The init container copies templates into an
			// emptyDir the server then reads — without this the pod crash-loops
			// on "permission denied" (observed on dev05).
			podSpec := deploy.Spec.Template.Spec
			c := podSpec.Containers[0]
			Expect(c.Image).To(Equal(defaultTemporalImage))

			Expect(podSpec.InitContainers).To(HaveLen(1))
			initC := podSpec.InitContainers[0]
			Expect(initC.Command[2]).To(ContainSubstring("cp -r " + temporalConfigDir + "/."))
			// Not -a: preserving ownership fails for an arbitrary UID, and GNU
			// coreutils turns that into a non-zero exit.
			Expect(initC.Command[2]).NotTo(ContainSubstring("cp -a"))

			// The halves have to be wired to each other, not merely both
			// present: v0.6.0 shipped an init container populating a volume the
			// server never mounted, and every assertion about the two in
			// isolation still passed while the pod crash-looped. Resolve the
			// volume the init container writes, then require the server to
			// mount that same volume over the path it reads.
			initTarget := mountPathFor(initC, temporalConfigVolume)
			Expect(initTarget).NotTo(BeEmpty(), "init container must populate the config volume")
			Expect(podSpec.Volumes).To(ContainElement(SatisfyAll(
				HaveField("Name", temporalConfigVolume),
				HaveField("VolumeSource.EmptyDir", Not(BeNil())),
			)), "the shared config volume must be an emptyDir any UID can write")
			Expect(mountPathFor(c, temporalConfigVolume)).To(Equal(temporalConfigDir),
				"the server must mount the very volume the init container populates, over the "+
					"config dir it reads — otherwise the templates go to %s and the server still "+
					"reads the unwritable image path", initTarget)

			env := map[string]corev1.EnvVar{}
			for _, e := range c.Env {
				env[e.Name] = e
			}
			Expect(env["POSTGRES_SEEDS"].Value).To(Equal(crName+"-openshift-sre-agent-postgresql"),
				"must reuse the PostgreSQL service the operator manages")
			Expect(env["DBNAME"].Value).To(Equal("temporal"))
			Expect(env["VISIBILITY_DBNAME"].Value).To(Equal("temporal_visibility"))
			Expect(env["POSTGRES_USER"].ValueFrom.SecretKeyRef.Name).To(Equal(crName+"-pg-auth"),
				"credentials come from the operator's pg auth secret, not a copy")
			Expect(env["POSTGRES_PWD"].ValueFrom.SecretKeyRef.Key).To(Equal("POSTGRESQL_PASSWORD"))
		})

		It("creates the Service the agent will dial", func() {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-temporal", Namespace: namespace}, svc)).To(Succeed())
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(7233)))
			Expect(TemporalHostFor(crName)).To(Equal(crName + "-temporal:7233"))
		})

		It("respects an image override", func() {
			cr.Spec.Temporal.Image = "quay.io/example/temporal:pinned"
			Expect(k8sClient.Update(ctx, cr)).To(Succeed())
			Expect(tr.reconcileTemporal(ctx, cr)).To(Succeed())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-temporal", Namespace: namespace}, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/example/temporal:pinned"))
		})
	})
})

// mountPathFor returns where container mounts the named volume, or "" if it
// does not mount it at all — the disconnected case this suite exists to catch.
func mountPathFor(container corev1.Container, volume string) string {
	for _, m := range container.VolumeMounts {
		if m.Name == volume {
			return m.MountPath
		}
	}
	return ""
}
