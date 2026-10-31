package controller

// One OpenShiftPulse per cluster, in one namespace.
//
// The operator installs for all namespaces, and OLM gives project admins and
// editors create access to the CRDs of an all-namespaces operator. On
// OpenShift any user can create a project and is its admin. Without this
// gate, that user could create an OpenShiftPulse with
// spec.agent.allowSecretAccess and allowWriteOperations, and the operator
// would bind the agent ServiceAccount in their project to cluster-wide
// Secret read and Deployment patch, which is a path to cluster-admin. A CR
// is the operator's trust boundary, so only one, in a namespace a cluster
// admin controls, is acted on.
//
// A CR anywhere else, or a second CR in the namespace, is rejected: nothing
// is created for it, and the cluster-scoped RBAC and OAuthClient it may hold
// from an earlier operator version are removed (they are what grant cluster
// reach). Its namespaced objects are left alone, so no data is deleted.

import (
	"context"
	"fmt"
	"os"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	// defaultPulseNamespace is where the one OpenShiftPulse lives unless the
	// install overrides it (PULSE_NAMESPACE on the manager, e.g. through the
	// Subscription's spec.config.env).
	defaultPulseNamespace = "openshiftpulse"
	envPulseNamespace     = "PULSE_NAMESPACE"

	phaseRejected   = "Rejected"
	reasonRejected  = "NotTheClusterInstance"
	conditionAccept = "Accepted"
)

// admissionEnabled turns the one-instance gate on. Only the envtest suite
// turns it off, because most of its specs create several CRs in "default";
// admission_test.go turns it back on for its own specs.
var admissionEnabled = true

// pulseNamespace is the only namespace whose OpenShiftPulse is reconciled.
func pulseNamespace() string {
	if ns := os.Getenv(envPulseNamespace); ns != "" {
		return ns
	}
	return defaultPulseNamespace
}

// rejectionReason says why pulse is not the cluster's Pulse instance, or ""
// when it is. Among several CRs in the namespace the oldest wins (name breaks
// a tie), so the answer is stable and an existing install keeps working.
func (r *OpenShiftPulseReconciler) rejectionReason(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (string, error) {
	if !admissionEnabled {
		return "", nil
	}
	ns := pulseNamespace()
	if pulse.Namespace != ns {
		return fmt.Sprintf("OpenShiftPulse is only supported in namespace %q, one per cluster; this one is in %q and is ignored. "+
			"A cluster admin can change the namespace with the operator's %s environment variable.", ns, pulse.Namespace, envPulseNamespace), nil
	}
	list := &pulsev1alpha1.OpenShiftPulseList{}
	if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
		return "", err
	}
	live := make([]pulsev1alpha1.OpenShiftPulse, 0, len(list.Items))
	for _, item := range list.Items {
		if item.DeletionTimestamp.IsZero() {
			live = append(live, item)
		}
	}
	if len(live) < 2 {
		return "", nil
	}
	sort.Slice(live, func(i, j int) bool {
		ti, tj := live[i].CreationTimestamp, live[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return live[i].Name < live[j].Name
	})
	if live[0].UID == pulse.UID {
		return "", nil
	}
	return fmt.Sprintf("Only one OpenShiftPulse is supported per cluster, and %s/%s already exists; this one is ignored.", ns, live[0].Name), nil
}

// reject records why pulse is ignored and removes the cluster-scoped grants it
// may hold from before this check existed.
func (r *OpenShiftPulseReconciler) reject(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, reason string) error {
	if err := r.deleteClusterScopedResources(ctx, pulse); err != nil {
		return fmt.Errorf("remove cluster-scoped resources of rejected OpenShiftPulse: %w", err)
	}
	if pulse.Status.Phase != phaseRejected && r.Recorder != nil {
		r.Recorder.Event(pulse, corev1.EventTypeWarning, reasonRejected, reason)
	}
	pulse.Status.Phase = phaseRejected
	apimeta.SetStatusCondition(&pulse.Status.Conditions, metav1.Condition{
		Type:               conditionAccept,
		Status:             metav1.ConditionFalse,
		Reason:             reasonRejected,
		Message:            reason,
		ObservedGeneration: pulse.Generation,
	})
	return r.Status().Update(ctx, pulse)
}
