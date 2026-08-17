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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	// Regression: reconcileUIDeployment resolves a stored Replicas value of 0 to
	// an effective 2 real pods (resolvedUIReplicas), but this reconciler used to
	// gate purely on the raw stored value, so it skipped the PDB whenever
	// Replicas was 0 -- exactly the case where two real, unprotected pods exist.
	// Set replicas via an unstructured Create with the key explicitly present as
	// 0 (not simply omitted) so CRD defaulting -- which only fires for absent
	// fields -- does not mask the raw-zero value being tested here.
	Describe("explicit spec.ui.replicas: 0", func() {
		const crName = "pdb-explicit-zero-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr); err == nil {
				_ = k8sClient.Delete(ctx, cr)
			}
		})

		It("still creates the PDB, matching the Deployment reconciler's 0-means-2 resolution", func() {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "pulse.ai", Version: "v1alpha1", Kind: "OpenShiftPulse"})
			obj.SetName(crName)
			obj.SetNamespace(namespace)
			Expect(unstructured.SetNestedMap(obj.Object, map[string]interface{}{
				"agent": map[string]interface{}{},
				"ui":    map[string]interface{}{"replicas": int64(0)},
			}, "spec")).To(Succeed())
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())

			cr := &pulsev1alpha1.OpenShiftPulse{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr)).To(Succeed())
			Expect(cr.Spec.UI.Replicas).To(Equal(int32(0)),
				"sanity check: an explicitly-present 0 must not be touched by CRD defaulting")

			Expect(rootRecon.reconcileUIPodsDisruptionBudget(ctx, cr)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      crName + "-openshiftpulse",
				Namespace: namespace,
			}, pdb)
			Expect(err).NotTo(HaveOccurred(),
				"PDB must exist: reconcileUIDeployment treats a stored Replicas of 0 as 2 real pods")
		})
	})
})
