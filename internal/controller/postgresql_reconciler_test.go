package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		// pg-auth deliberately has no OwnerReference (see reconcilePGSecret's
		// doc comment) so it survives CR deletion by design — clean it up
		// explicitly here so it doesn't leak into other specs in this suite
		// that reuse crName.
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: crName + "-pg-auth", Namespace: namespace}})
	})

	It("reconcilePostgres creates Secret, StatefulSet, and Service", func() {
		_, _, err := pg.reconcilePostgres(ctx, cr)
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
		_, _, err := pg.reconcilePostgres(ctx, cr)
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
		_, _, err := pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		secret1 := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-auth",
			Namespace: namespace,
		}, secret1)).To(Succeed())
		pass1 := string(secret1.Data["POSTGRESQL_PASSWORD"])

		_, _, err = pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		secret2 := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-auth",
			Namespace: namespace,
		}, secret2)).To(Succeed())
		Expect(string(secret2.Data["POSTGRESQL_PASSWORD"])).To(Equal(pass1))
	})

	// Regression: a selector-mismatched StatefulSet (e.g. previously
	// Helm-managed) used to be handled by deleting it and returning a
	// synthetic error purely to force a requeue, which controller-runtime
	// turns into a Warning ReconcileFailed event with exponential backoff —
	// making a normal, successful migration step look like a failing operator.
	It("deletes a selector-mismatched StatefulSet and requeues cleanly, without an error", func() {
		stsName := crName + "-openshift-sre-agent-postgresql"

		// envtest does not run the garbage collector controller, so a real
		// StatefulSet left behind by another spec in this suite (owned by
		// the same CR name, never cascade-deleted) can still exist here.
		// Clear it first so the fake "pre-existing, mismatched" object below
		// can be created cleanly regardless of run order.
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: namespace}},
			client.PropagationPolicy(metav1.DeletePropagationBackground))
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, &appsv1.StatefulSet{}))
		}).Should(BeTrue())

		one := int32(1)
		preExisting := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: namespace},
			Spec: appsv1.StatefulSetSpec{
				Replicas:    &one,
				ServiceName: stsName + "-headless",
				Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "helm-managed-pg"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "helm-managed-pg"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgresql", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, preExisting)).To(Succeed())

		result, err := pg.reconcilePGStatefulSet(ctx, cr, stsName, crName+"-pg-auth")
		Expect(err).NotTo(HaveOccurred(), "a selector mismatch is an expected migration step, not a failure")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "must signal a requeue instead of relying on an error")

		sts := &appsv1.StatefulSet{}
		getErr := k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, sts)
		Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "mismatched StatefulSet must actually have been deleted")
	})

	It("requeues cleanly (without an error) while a StatefulSet is still terminating", func() {
		stsName := crName + "-openshift-sre-agent-postgresql"

		// Same run-order concern as the mismatch test above: clear out any
		// real StatefulSet left behind by an earlier spec before creating
		// the fake finalizer-blocked one this test needs.
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: namespace}},
			client.PropagationPolicy(metav1.DeletePropagationBackground))
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, &appsv1.StatefulSet{}))
		}).Should(BeTrue())

		one := int32(1)
		preExisting := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:       stsName,
				Namespace:  namespace,
				Finalizers: []string{"pulse.ai/test-block-deletion"}, // keeps it around post-Delete for this test
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas:    &one,
				ServiceName: stsName + "-headless",
				Selector:    &metav1.LabelSelector{MatchLabels: pgLabels(crName)},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: pgLabels(crName)},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgresql", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, preExisting)).To(Succeed())
		Expect(k8sClient.Delete(ctx, preExisting)).To(Succeed())
		DeferCleanup(func() {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, sts); err == nil {
				sts.Finalizers = nil
				_ = k8sClient.Update(ctx, sts)
			}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, &appsv1.StatefulSet{}))
			}).Should(BeTrue(), "test STS must be fully gone before the next spec runs")
		})

		result, err := pg.reconcilePGStatefulSet(ctx, cr, stsName, crName+"-pg-auth")
		Expect(err).NotTo(HaveOccurred(), "waiting for termination is an expected step, not a failure")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})

	// Regression: the {name}-postgresql connection secret's database-url was
	// create-only. If the underlying pg-auth password ever legitimately
	// changes (e.g. the legacy POSTGRES_*->POSTGRESQL_* migration path in
	// reconcilePGSecret, or any future rotation), the connection secret kept
	// serving a stale URL forever, since nothing ever refreshed it.
	It("refreshes database-url in the connection secret when the underlying password changes", func() {
		_, _, err := pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		connSecretName := crName + "-postgresql"
		before := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connSecretName, Namespace: namespace}, before)).To(Succeed())
		staleURL := string(before.Data["database-url"])
		Expect(staleURL).NotTo(BeEmpty())

		// Simulate the password having legitimately changed underneath
		// (e.g. an external rotation) by editing pg-auth directly.
		pgAuth := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-pg-auth", Namespace: namespace}, pgAuth)).To(Succeed())
		pgAuth.Data["POSTGRESQL_PASSWORD"] = []byte("rotated-password-xyz")
		Expect(k8sClient.Update(ctx, pgAuth)).To(Succeed())

		_, _, err = pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		after := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connSecretName, Namespace: namespace}, after)).To(Succeed())
		Expect(string(after.Data["database-url"])).NotTo(Equal(staleURL),
			"database-url must be refreshed to match the current pg-auth password, not left stale")
		Expect(string(after.Data["database-url"])).To(ContainSubstring("rotated-password-xyz"))
	})

	// Regression: recreating a CR with the same name after deletion used to
	// generate a fresh random pg-auth password every time (the Secret had an
	// OwnerReference and was garbage-collected with the CR), but the PG data
	// PVC has no retention policy and is intentionally retained — postgres
	// only runs initdb (which bakes in a password) on an empty data
	// directory, so the new random password would never match what's
	// already on the retained volume, and the agent would get permanent
	// authentication failures with no self-heal. Not owning the Secret means
	// it survives right alongside the retained PVC and gets correctly reused.
	It("reuses the same pg-auth password across a CR delete+recreate cycle with the same name", func() {
		_, _, err := pg.reconcilePostgres(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		pgAuth := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-pg-auth", Namespace: namespace}, pgAuth)).To(Succeed())
		originalPassword := string(pgAuth.Data["POSTGRESQL_PASSWORD"])
		Expect(originalPassword).NotTo(BeEmpty())
		Expect(pgAuth.GetOwnerReferences()).To(BeEmpty(),
			"pg-auth must have no OwnerReference so a real cluster's garbage collector never removes it on CR deletion")

		// Delete the CR (not pg-auth) and recreate a fresh CR object with the
		// same name — simulating exactly the scenario that used to strand
		// the agent: same name, freshly-generated CR, but pg-auth (and the
		// retained PVC it matches) still around from before.
		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		recreated := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Database: pulsev1alpha1.DatabaseConfig{StorageSize: "5Gi"},
			},
		}
		Expect(k8sClient.Create(ctx, recreated)).To(Succeed())
		cr = recreated // let AfterEach clean up the recreated object

		_, _, err = pg.reconcilePostgres(ctx, recreated)
		Expect(err).NotTo(HaveOccurred())

		afterRecreate := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-pg-auth", Namespace: namespace}, afterRecreate)).To(Succeed())
		Expect(string(afterRecreate.Data["POSTGRESQL_PASSWORD"])).To(Equal(originalPassword),
			"the recreated CR must get the SAME password back, matching what's already initialized on the retained PG data volume")
	})

	// Regression coverage for the explicit opt-in teardown path: without
	// annotationDeleteData, pg-auth and the data PVC must NOT be touched by
	// deletePGDataOnRequest; with it, both must actually be deleted.
	Describe("deletePGDataOnRequest", func() {
		var pvcName string

		BeforeEach(func() {
			pvcName = "pg-data-" + crName + "-openshift-sre-agent-postgresql-0"
		})

		It("does nothing without the delete-data annotation", func() {
			_, _, err := pg.reconcilePostgres(ctx, cr)
			Expect(err).NotTo(HaveOccurred())

			Expect(pg.deletePGDataOnRequest(ctx, cr)).To(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-pg-auth", Namespace: namespace}, &corev1.Secret{})).To(Succeed(),
				"pg-auth must survive when the CR did not opt into a full teardown")
		})

		It("deletes pg-auth and the data PVC when annotationDeleteData is set", func() {
			_, _, err := pg.reconcilePostgres(ctx, cr)
			Expect(err).NotTo(HaveOccurred())

			// The data PVC normally comes from the StatefulSet's
			// volumeClaimTemplates, materialized by the StatefulSet
			// controller — which doesn't run in envtest (only the API
			// server + etcd do). Create the PVC object directly at the name
			// deletePGDataOnRequest expects, so this test can deterministically
			// exercise the actual delete call rather than depend on
			// unavailable controller behavior.
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: namespace},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())

			cr.Annotations = map[string]string{annotationDeleteData: "true"}
			Expect(k8sClient.Update(ctx, cr)).To(Succeed())

			Expect(pg.deletePGDataOnRequest(ctx, cr)).To(Succeed())

			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: crName + "-pg-auth", Namespace: namespace}, &corev1.Secret{}))).To(BeTrue(),
				"pg-auth must be deleted when the CR explicitly opted into a full teardown")

			// A real Delete was requested (proves deletePGDataOnRequest
			// actually called it): envtest's API server auto-adds the
			// kubernetes.io/pvc-protection finalizer, which only the (not
			// running in envtest) pv-protection controller ever removes, so
			// the PVC stays in Terminating rather than fully disappearing
			// here — strip it manually to assert full deletion rather than
			// just a DeletionTimestamp.
			afterDelete := &corev1.PersistentVolumeClaim{}
			getErr := k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, afterDelete)
			if getErr == nil {
				Expect(afterDelete.DeletionTimestamp).NotTo(BeNil(), "the data PVC must have been marked for deletion")
				afterDelete.Finalizers = nil
				Expect(k8sClient.Update(ctx, afterDelete)).To(Succeed())
			} else {
				Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
			}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, &corev1.PersistentVolumeClaim{}))
			}).Should(BeTrue(), "the data PVC must be fully deleted when the CR explicitly opted into a full teardown")
		})
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
