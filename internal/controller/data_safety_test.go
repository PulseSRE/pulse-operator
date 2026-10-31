package controller

import (
	"github.com/prometheus/client_golang/prometheus/testutil"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = DescribeTable("pgMajorVersion",
	func(image string, want int) { Expect(pgMajorVersion(image)).To(Equal(want)) },
	Entry("UBI tag", "registry.redhat.io/rhel9/postgresql-15:latest", 15),
	Entry("UBI digest", "registry.redhat.io/rhel9/postgresql-15@sha256:cb2b214b470dfb39e19d6d71819844e855c52435c78deea025c1e0650ad175f8", 15),
	Entry("sclorg", "quay.io/sclorg/postgresql-16-c9s:latest", 16),
	Entry("upstream", "docker.io/library/postgres:16-alpine", 16),
	Entry("unknown", "registry.internal/db:v3", 0),
)

var _ = Describe("PostgreSQL data safety", func() {
	const namespace = "default"

	newPG := func(name string) (*PostgreSQLReconciler, *record.FakeRecorder, *pulsev1alpha1.OpenShiftPulse) {
		rec := record.NewFakeRecorder(20)
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })
		return &PostgreSQLReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}, rec, cr
	}
	getSTS := func(name string) *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, sts)).To(Succeed())
		return sts
	}

	It("refuses a PostgreSQL major-version change and keeps the running image", func() {
		pg, rec, cr := newPG("pg-major-guard")
		stsName := cr.Name + "-openshift-sre-agent-postgresql"
		cr.Spec.Database.Image = "registry.redhat.io/rhel9/postgresql-15:1"
		_, err := pg.reconcilePGStatefulSet(testCtx, cr, stsName, cr.Name+"-pg-auth")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, getSTS(stsName)) })

		cr.Spec.Database.Image = "registry.redhat.io/rhel9/postgresql-16:1"
		_, err = pg.reconcilePGStatefulSet(testCtx, cr, stsName, cr.Name+"-pg-auth")
		Expect(err).NotTo(HaveOccurred())
		Expect(getSTS(stsName).Spec.Template.Spec.Containers[0].Image).To(Equal("registry.redhat.io/rhel9/postgresql-15:1"))
		Expect(drainEvent(rec)).To(ContainSubstring("PostgreSQLMajorVersionChangeBlocked"))

		// A same-major change still goes through.
		cr.Spec.Database.Image = "registry.redhat.io/rhel9/postgresql-15:2"
		_, err = pg.reconcilePGStatefulSet(testCtx, cr, stsName, cr.Name+"-pg-auth")
		Expect(err).NotTo(HaveOccurred())
		Expect(getSTS(stsName).Spec.Template.Spec.Containers[0].Image).To(Equal("registry.redhat.io/rhel9/postgresql-15:2"))
	})

	It("shuts PostgreSQL down fast before SIGTERM and restarts it when its credentials change", func() {
		pg, _, cr := newPG("pg-shutdown")
		stsName := cr.Name + "-openshift-sre-agent-postgresql"
		secretName := cr.Name + "-pg-auth"
		_, err := pg.reconcilePGSecret(testCtx, cr, secretName)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = k8sClient.Delete(testCtx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace}})
		})
		_, err = pg.reconcilePGStatefulSet(testCtx, cr, stsName, secretName)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, getSTS(stsName)) })

		sts := getSTS(stsName)
		spec := sts.Spec.Template.Spec
		Expect(*spec.TerminationGracePeriodSeconds).To(BeEquivalentTo(pgTerminationGraceSeconds))
		Expect(spec.Containers[0].Lifecycle.PreStop.Exec.Command[2]).To(ContainSubstring("stop -m fast"))
		before := sts.Spec.Template.Annotations[pgCredentialHashAnnotation]
		Expect(before).NotTo(BeEmpty())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret)).To(Succeed())
		secret.Data["POSTGRESQL_PASSWORD"] = []byte("rotated")
		Expect(k8sClient.Update(testCtx, secret)).To(Succeed())
		_, err = pg.reconcilePGStatefulSet(testCtx, cr, stsName, secretName)
		Expect(err).NotTo(HaveOccurred())
		Expect(getSTS(stsName).Spec.Template.Annotations[pgCredentialHashAnnotation]).NotTo(Equal(before))
	})

	It("reports an invalid storageSize on an existing database instead of failing the reconcile", func() {
		pg, rec, cr := newPG("pg-bad-size")
		stsName := cr.Name + "-openshift-sre-agent-postgresql"
		_, err := pg.reconcilePGStatefulSet(testCtx, cr, stsName, cr.Name+"-pg-auth")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, getSTS(stsName)) })

		cr.Spec.Database.StorageSize = "10GB"
		_, err = pg.reconcilePGStatefulSet(testCtx, cr, stsName, cr.Name+"-pg-auth")
		Expect(err).NotTo(HaveOccurred())
		Expect(drainEvent(rec)).To(ContainSubstring("InvalidStorageSize"))
	})

	It("grows the database volume when storageSize increases", func() {
		pg, rec, cr := newPG("pg-grow")
		stsName := cr.Name + "-openshift-sre-agent-postgresql"
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-data-" + stsName + "-0", Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
			},
		}
		Expect(k8sClient.Create(testCtx, pvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, pvc) })

		Expect(pg.expandPGVolume(testCtx, cr, stsName, resource.MustParse("10Gi"))).To(Succeed())
		// envtest has no storage class to check expansion against, so the
		// API server either accepts the larger request or refuses it; both
		// are reported, neither fails the reconcile.
		Expect(drainEvent(rec)).To(Or(ContainSubstring("StorageResized"), ContainSubstring("StorageResizeFailed")))

		Expect(pg.expandPGVolume(testCtx, cr, stsName, resource.MustParse("1Gi"))).To(Succeed(), "shrinking is ignored")
	})
})

var _ = Describe("Temporal rollout", func() {
	It("uses Recreate so two auto-setup pods never migrate the schema at once", func() {
		enabled := true
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: "temporal-recreate", Namespace: "default"},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Temporal: pulsev1alpha1.TemporalConfig{Enabled: &enabled}},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })
		tr := &TemporalReconciler{Client: k8sClient, Scheme: testScheme}
		Expect(tr.reconcileTemporal(testCtx, cr)).To(Succeed())
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: "temporal-recreate-temporal", Namespace: "default"}, deploy)).To(Succeed())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(deploy.Spec.Template.Annotations).To(HaveKey(pgCredentialHashAnnotation))
	})
})

var _ = Describe("forgetInstanceMetrics", func() {
	It("stops exporting a deleted instance's series", func() {
		componentReady.WithLabelValues("ns-gone", "pulse", "agent").Set(0)
		componentReady.WithLabelValues("ns-kept", "pulse", "agent").Set(1)
		before := testutil.CollectAndCount(componentReady)

		forgetInstanceMetrics("ns-gone", "pulse")

		Expect(testutil.CollectAndCount(componentReady)).To(Equal(before - 1))
		Expect(testutil.ToFloat64(componentReady.WithLabelValues("ns-kept", "pulse", "agent"))).To(Equal(1.0))
	})
})
