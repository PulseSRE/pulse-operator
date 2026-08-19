package controller

// Item 1 of the autonomy roadmap: generalizes the PostgreSQL StatefulSet's
// stale-pod self-heal (postgresql_reconciler.go's isPGPodStale /
// deletePendingPGPodIfStale) to the agent and UI Deployments. Unlike the
// StatefulSet's single, deterministically-named pod-0, a Deployment's pods
// have generated names and (for the UI in particular) there may be more
// than one — so this looks pods up by the Deployment's own "app" label
// selector rather than a fixed name, and can delete more than one stale pod
// per call.
//
// This matters most for the agent, whose Deployment uses the Recreate
// strategy (see agent_reconciler.go's buildDeploymentSpec and item 4's
// investigation in this same PR): Recreate tears the old pod down before
// the new one starts, so if the new pod hits ImagePullBackOff or
// CrashLoopBackOff there is no old pod left to fall back to — without this,
// a bad rollout is stuck until a human notices and runs `oc delete pod`
// by hand.

import (
	"context"
	goerrors "errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// staleWaitingReasons are the container Waiting.Reason values that mark a
// Deployment-managed pod as stuck badly enough to self-heal with no grace
// period. This is deliberately different from the long-Pending case (see
// pgPendingPodStaleThreshold's doc comment in postgresql_reconciler.go —
// that grace period exists because every pod is transiently Pending for a
// few seconds to tens of seconds during entirely normal
// scheduling/volume-attach/image-pull). ImagePullBackOff and
// CrashLoopBackOff are different in kind, not just degree: per
// Pod.Status.ContainerStatuses[].State.Waiting semantics, the kubelet only
// ever reports one of these *after* it has already made and failed at
// least one real attempt — a pull attempt for the former, a full
// start-then-exit cycle for the latter. There is no "might just be
// mid-startup" ambiguity left to wait out, so reacting immediately (rather
// than waiting out a grace period the way Pending needs) is safe.
//
// This does mean a persistently-broken replacement pod (e.g. a genuinely
// bad image tag that will never pull) can be deleted and recreated again
// on a later reconcile. That is bounded, not a repeat of the incident
// pgPendingPodStaleThreshold exists to prevent: this operator's steady-state
// reconcile cadence is 30s (OpenShiftPulseReconciler.Reconcile's
// RequeueAfter), so at worst this repeats once per reconcile interval —
// the same cadence a human running `oc delete pod` while debugging would
// produce by hand — not an unbounded, sub-second create-delete loop.
var staleWaitingReasons = map[string]bool{
	"ImagePullBackOff": true,
	"CrashLoopBackOff": true,
}

// isDeploymentPodStale reports whether pod is stuck badly enough for
// deleteStalePodsForDeployment to remove it. now is taken as a parameter
// (not time.Now() internally) for the same reason as isPGPodStale: it lets
// the Pending-threshold case be unit tested without depending on a real,
// server-assigned CreationTimestamp.
func isDeploymentPodStale(pod *corev1.Pod, now time.Time) bool {
	if pod.Status.Phase == corev1.PodPending && now.Sub(pod.CreationTimestamp.Time) >= pgPendingPodStaleThreshold {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && staleWaitingReasons[cs.State.Waiting.Reason] {
			return true
		}
	}
	return false
}

// stalePodReasonDescription renders a short human-readable reason for the
// SelfHealed event message.
func stalePodReasonDescription(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && staleWaitingReasons[cs.State.Waiting.Reason] {
			return cs.State.Waiting.Reason
		}
	}
	return "Pending too long"
}

// deleteStalePodsForDeployment lists the pods matching a Deployment's own
// pod-template selector ({"app": deploymentName} — see
// AgentReconciler.buildDeploymentSpec / UIReconciler.reconcileUIDeployment)
// in pulse's namespace, and deletes every one isDeploymentPodStale reports
// stuck, so the Deployment/ReplicaSet controller creates a fresh
// replacement and retries the pull or start. component/action feed
// selfHealActionsTotal's labels (see metrics.go), matching the existing
// self-heal sites' convention.
//
// Errors deleting one pod do not stop the rest — best-effort, matching
// deletePendingPGPodIfStale's "non-fatal, next reconcile retries" contract
// at its call sites (agent_reconciler.go / ui_reconciler.go).
func deleteStalePodsForDeployment(
	ctx context.Context,
	c client.Client,
	recorder record.EventRecorder,
	pulse *pulsev1alpha1.OpenShiftPulse,
	deploymentName, component, action string,
) error {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList,
		client.InNamespace(pulse.Namespace),
		client.MatchingLabels{"app": deploymentName},
	); err != nil {
		return err
	}

	now := time.Now()
	var errs []error
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil || !isDeploymentPodStale(pod, now) {
			continue
		}
		reason := stalePodReasonDescription(pod)
		if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		recordEvent(recorder, pulse, corev1.EventTypeNormal, "SelfHealed",
			"%s pod %q was stuck (%s) — deleted it so the Deployment controller can create a fresh one",
			component, pod.Name, reason)
		selfHealActionsTotal.WithLabelValues(component, action).Inc()
	}
	return goerrors.Join(errs...)
}
