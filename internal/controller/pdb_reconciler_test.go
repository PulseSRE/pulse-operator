package controller

// PDBReconciler had zero test coverage before this file — flagged in AUDIT.md
// and still true as of the independent review that added this file.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = Describe("PDBReconciler (reconcileUIPodsDisruptionBudget)", func() {
	const namespace = "default"

	var (
		ctx       context.Context
		rootRecon *OpenShiftPulseReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		rootRecon = &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme}
	})

	Describe("single UI replica", func() {
		const crName = "pdb-single-pulse"
		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					UI: pulsev1alpha1.UIConfig{Replicas: 1},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() { _ = k8sClient.Delete(ctx, cr) })

		It("does not create a PodDisruptionBudget when replicas <= 1", func() {
			Expect(rootRecon.reconcileUIPodsDisruptionBudget(ctx, cr)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      crName + "-openshiftpulse",
				Namespace: namespace,
			}, pdb)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PDB must not exist for a single replica")
		})
	})

	Describe("multiple UI replicas", func() {
		const crName = "pdb-multi-pulse"
		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					UI: pulsev1alpha1.UIConfig{Replicas: 2},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() { _ = k8sClient.Delete(ctx, cr) })

		It("creates a PodDisruptionBudget with minAvailable=1 selecting the UI pods", func() {
			Expect(rootRecon.reconcileUIPodsDisruptionBudget(ctx, cr)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      crName + "-openshiftpulse",
				Namespace: namespace,
			}, pdb)).To(Succeed())

			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app", crName+"-openshiftpulse"))
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(1))
		})

		It("PDB has an OwnerReference to the CR", func() {
			Expect(rootRecon.reconcileUIPodsDisruptionBudget(ctx, cr)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      crName + "-openshiftpulse",
				Namespace: namespace,
			}, pdb)).To(Succeed())
			Expect(pdb.GetOwnerReferences()).NotTo(BeEmpty())
		})

		It("reconcileUIPodsDisruptionBudget is idempotent", func() {
			Expect(rootRecon.reconcileUIPodsDisruptionBudget(ctx, cr)).To(Succeed())
			Expect(rootRecon.reconcileUIPodsDisruptionBudget(ctx, cr)).To(Succeed())
		})
	})
})
