package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// Any OpenShift user can create a project, and OLM gives project admins
// create access to an all-namespaces operator's CRDs. An OpenShiftPulse in
// their project used to get cluster-wide Secret read and Deployment patch
// bound to a ServiceAccount they control.
var _ = Describe("One OpenShiftPulse per cluster", func() {
	const (
		pulseNS = "admission-pulse"
		userNS  = "admission-user-project"
	)

	BeforeEach(func() {
		admissionEnabled = true
		GinkgoT().Setenv(envPulseNamespace, pulseNS)
		for _, ns := range []string{pulseNS, userNS} {
			err := k8sClient.Create(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		}
	})
	AfterEach(func() { admissionEnabled = false })

	root := func() *OpenShiftPulseReconciler {
		return &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(20)}
	}
	create := func(ns, name string) *pulsev1alpha1.OpenShiftPulse {
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		cr.Spec.Agent.AllowSecretAccess = true
		cr.Spec.Agent.AllowWriteOperations = true
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() {
			got := &pulsev1alpha1.OpenShiftPulse{}
			if k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, got) == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(testCtx, got)
				_ = k8sClient.Delete(testCtx, got)
			}
		})
		return cr
	}
	reconcile := func(cr *pulsev1alpha1.OpenShiftPulse) ctrl.Result {
		res, err := root().Reconcile(testCtx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	accepted := func(cr *pulsev1alpha1.OpenShiftPulse) *metav1.Condition {
		got := &pulsev1alpha1.OpenShiftPulse{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, got)).To(Succeed())
		return apimeta.FindStatusCondition(got.Status.Conditions, conditionAccept)
	}

	It("ignores a CR in a user's project and grants it nothing", func() {
		cr := create(userNS, "pulse")
		res := reconcile(cr)
		Expect(res.RequeueAfter).To(Equal(5 * time.Minute))

		cond := accepted(cr)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring(pulseNS))

		err := k8sClient.Get(testCtx, types.NamespacedName{Name: agentClusterRoleName(cr.Name, cr.Namespace)}, &rbacv1.ClusterRole{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no cluster-wide role for a rejected CR")
		got := &pulsev1alpha1.OpenShiftPulse{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, got)).To(Succeed())
		Expect(got.Finalizers).To(BeEmpty())
	})

	It("removes cluster-wide grants a rejected CR holds from an earlier operator", func() {
		cr := create(userNS, "legacy")
		role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
			Name:        agentClusterRoleName(cr.Name, cr.Namespace),
			Annotations: map[string]string{annotationOwnerUID: string(cr.UID)},
		}}
		Expect(k8sClient.Create(testCtx, role)).To(Succeed())

		reconcile(cr)

		err := k8sClient.Get(testCtx, types.NamespacedName{Name: role.Name}, &rbacv1.ClusterRole{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("accepts the first CR in the Pulse namespace and rejects a second", func() {
		first := create(pulseNS, "pulse")
		time.Sleep(1100 * time.Millisecond) // creationTimestamp has second precision
		second := create(pulseNS, "pulse-two")

		reason, err := root().rejectionReason(testCtx, first)
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(BeEmpty())

		reconcile(second)
		cond := accepted(second)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("Only one OpenShiftPulse"))
	})
})
