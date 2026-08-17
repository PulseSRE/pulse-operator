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

var _ = Describe("PostgreSQLReconciler", func() {
	const (
		crName    = "pg-test-pulse"
		namespace = "default"
	)

	var (
		cr  *pulsev1alpha1.OpenShiftPulse
		ctx context.Context
		pg  *PostgreSQLReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		pg = &PostgreSQLReconciler{Client: k8sClient, Scheme: testScheme}

		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Database: pulsev1alpha1.DatabaseConfig{StorageSize: "5Gi"},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, cr)
	})

	It("reconcilePostgres creates Secret, StatefulSet, and Service", func() {
		_, err := pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		// Secret
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-auth",
			Namespace: namespace,
		}, secret)).To(Succeed())

		// StatefulSet
		sts := &appsv1.StatefulSet{}
		stsName := crName + "-openshift-sre-agent-postgresql"
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      stsName,
			Namespace: namespace,
		}, sts)).To(Succeed())

		// ClusterIP Service
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      stsName,
			Namespace: namespace,
		}, svc)).To(Succeed())
	})

	It("Secret uses POSTGRESQL_* keys, not POSTGRES_*", func() {
		_, err := pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-auth",
			Namespace: namespace,
		}, secret)).To(Succeed())

		_, hasUser := secret.Data["POSTGRESQL_USER"]
		_, hasPass := secret.Data["POSTGRESQL_PASSWORD"]
		_, hasDB := secret.Data["POSTGRESQL_DATABASE"]
		Expect(hasUser).To(BeTrue(), "must have POSTGRESQL_USER key")
		Expect(hasPass).To(BeTrue(), "must have POSTGRESQL_PASSWORD key")
		Expect(hasDB).To(BeTrue(), "must have POSTGRESQL_DATABASE key")

		_, oldUser := secret.Data["POSTGRES_USER"]
		_, oldPass := secret.Data["POSTGRES_PASSWORD"]
		_, oldDB := secret.Data["POSTGRES_DB"]
		Expect(oldUser).To(BeFalse(), "must NOT have legacy POSTGRES_USER key")
		Expect(oldPass).To(BeFalse(), "must NOT have legacy POSTGRES_PASSWORD key")
		Expect(oldDB).To(BeFalse(), "must NOT have legacy POSTGRES_DB key")
	})

	It("password is not rotated on second reconcile", func() {
		_, err := pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		secret1 := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-auth",
			Namespace: namespace,
		}, secret1)).To(Succeed())
		pass1 := string(secret1.Data["POSTGRESQL_PASSWORD"])

		_, err = pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		secret2 := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-auth",
			Namespace: namespace,
		}, secret2)).To(Succeed())
		Expect(string(secret2.Data["POSTGRESQL_PASSWORD"])).To(Equal(pass1))
	})

	It("reconcilePGService is idempotent", func() {
		svcName := crName + "-openshift-sre-agent-postgresql"

		Expect(pg.reconcilePGService(ctx, cr, svcName, false)).To(Succeed())
		Expect(pg.reconcilePGService(ctx, cr, svcName, false)).To(Succeed())

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      svcName,
			Namespace: namespace,
		}, svc)).To(Succeed())
	})
})
