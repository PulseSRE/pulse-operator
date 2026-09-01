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
			Expect(deploy.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(deploy.Spec.Template.Spec.InitContainers[0].Command[2]).To(ContainSubstring("cp -a /etc/temporal/config/."))
			Expect(deploy.Spec.Template.Spec.Volumes[0].Name).To(Equal("temporal-config"))

			c := deploy.Spec.Template.Spec.Containers[0]
			Expect(c.VolumeMounts[0].MountPath).To(Equal("/etc/temporal/config"))
			Expect(c.Image).To(Equal(defaultTemporalImage))

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
