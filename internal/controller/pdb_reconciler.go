package controller

import (
	"context"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// reconcileUIPodsDisruptionBudget creates or updates a PodDisruptionBudget for the UI
// deployment. The PDB is only created when the UI Deployment will actually run more
// than one replica; with a single replica the PDB would block node drains without
// providing any availability benefit. Uses resolvedUIReplicas (not the raw spec field)
// so a CR that omits spec.ui.replicas — which the Deployment reconciler resolves to the
// default of 2 — still gets the PDB its two real pods need, instead of silently getting
// none.
func (r *OpenShiftPulseReconciler) reconcileUIPodsDisruptionBudget(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := pulse.Name + "-openshiftpulse"
	if resolvedUIReplicas(pulse) <= 1 {
		// Scaling to one replica must also remove the PDB made for two:
		// minAvailable=1 over a single pod can never be satisfied by an
		// eviction, so node drains (and OCP upgrades) stall on its node.
		existing := &policyv1.PodDisruptionBudget{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !metav1.IsControlledBy(existing, pulse) {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, existing))
	}

	uiApp := pulse.Name + "-openshiftpulse"
	minAvailable := intstr.FromInt(1)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": uiApp},
		}
		pdb.Spec.MinAvailable = &minAvailable
		return controllerutil.SetControllerReference(pulse, pdb, r.Scheme)
	})
	return err
}
