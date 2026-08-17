package controller

import (
	"context"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	if resolvedUIReplicas(pulse) <= 1 {
		return nil
	}

	name := pulse.Name + "-openshiftpulse"
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
