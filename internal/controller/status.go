package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// upgradeHealthTimeout bounds how long an in-progress agent/UI image change
// (Phase=Upgrading) is given to reach Ready before reconcileAutoRollback
// reverts spec.agent.image/spec.ui.image back to the last known-healthy
// value. Sized comfortably above the agent's own probe timings (Recreate
// strategy means a full pod teardown + image pull before the
// ReadinessProbe's InitialDelaySeconds=5/PeriodSeconds=10 can even start
// counting successes) while still catching a bad rollout within one
// incident-response cycle instead of paging a human first.
const upgradeHealthTimeout = 5 * time.Minute

// componentHealth is what the status sync needs to know about the agent,
// UI and database.
//
// Serving and rolled out are different questions. A component is serving
// when some pod is ready; it is rolled out when the Deployment has caught up
// with its current spec and every pod runs it. Only "rolled out" says the
// image asked for works. Recording last-healthy from "serving" recorded
// every new image as healthy in the same pass that applied it (the old pod
// was still ready), so rollback never fired for the agent, and never could
// for the UI, whose old pods stay up through a rolling update.
type componentHealth struct {
	agentServing, agentRolledOut bool
	uiRolledOut                  bool
	pgReady                      bool
}

// syncPhaseAndConditions computes pulse.Status.Phase and the AgentReady /
// DatabaseReady / UIReady / Progressing / Ready conditions from the current
// component-health booleans. It distinguishes an in-progress image upgrade
// (Phase=Upgrading) from an unplanned failure (Phase=Degraded) by comparing
// each component's desired spec image against the last image observed
// healthy (status.lastHealthy{Agent,UI}Image) — without this, a routine
// `spec.agent.image` bump and a genuine outage were indistinguishable to
// anything watching Phase.
//
// Returns the resolved desired agent/UI images and whether each is
// currently "upgrading" (spec asks for an image that differs from its
// last-healthy value, and that component isn't ready yet) —
// reconcileAutoRollback uses these two booleans to decide what, if
// anything, to roll back once upgradeHealthTimeout is exceeded.
func (r *OpenShiftPulseReconciler) syncPhaseAndConditions(
	pulse *pulsev1alpha1.OpenShiftPulse,
	h componentHealth,
) (agentImage, uiImage string, agentUpgrading, uiUpgrading bool) {
	agentReady, pgReady := h.agentServing, h.pgReady
	uiReady := pulse.Status.UIAvailable
	// A rollback record stops applying once the spec asks for a different
	// image; drop it so status says what is running and why.
	if rb := pulse.Status.AgentRollback; rb != nil && rb.From != requestedImage(pulse) {
		pulse.Status.AgentRollback = nil
	}
	if rb := pulse.Status.UIRollback; rb != nil && rb.From != requestedUIImage(pulse) {
		pulse.Status.UIRollback = nil
	}
	agentImage = resolvedImage(pulse)
	uiImage = resolvedUIImage(pulse)

	// Snapshot before the UpgradeStartedAt-clearing logic below runs, so the
	// allReady branch can still compute how long the upgrade that just
	// completed actually took (see LastUpgradeDurationSeconds below) even
	// though the live field gets cleared to nil in the same pass.
	previousUpgradeStartedAt := pulse.Status.UpgradeStartedAt

	agentUpgrading = !h.agentRolledOut && pulse.Status.LastHealthyAgentImage != "" &&
		agentImage != pulse.Status.LastHealthyAgentImage
	uiUpgrading = !h.uiRolledOut && pulse.Status.LastHealthyUIImage != "" &&
		uiImage != pulse.Status.LastHealthyUIImage

	// Only record an image as "known healthy" once its Deployment has rolled
	// out with it — this is what makes the comparison above mean "differs
	// from the last image that worked", not just "differs from whatever was
	// there before".
	if h.agentRolledOut && agentImage != "" {
		pulse.Status.LastHealthyAgentImage = agentImage
	}
	if h.uiRolledOut && uiImage != "" {
		pulse.Status.LastHealthyUIImage = uiImage
	}

	allReady := agentReady && pgReady && uiReady
	upgrading := agentUpgrading || uiUpgrading

	if upgrading && pulse.Status.UpgradeStartedAt == nil {
		now := metav1.Now()
		pulse.Status.UpgradeStartedAt = &now
	} else if !upgrading {
		pulse.Status.UpgradeStartedAt = nil
	}

	prevPhase := pulse.Status.Phase
	switch {
	case upgrading && allReady:
		// Still serving from the old pods (the UI's rolling update keeps
		// them) while the new image has not rolled out: an upgrade in
		// progress, which reconcileAutoRollback times out like any other.
		pulse.Status.Phase = "Upgrading"
		if prevPhase != "Upgrading" {
			recordEvent(r.Recorder, pulse, corev1.EventTypeNormal, "Upgrading",
				"Detected an in-progress image change: agentImage=%q uiImage=%q", agentImage, uiImage)
		}
	case allReady:
		pulse.Status.Phase = "Running"
		// An upgrade that was in progress a moment ago just became healthy
		// — record how long the (unavoidable, for the agent's Recreate
		// strategy — see item 4's investigation) outage window actually
		// was. previousUpgradeStartedAt (not pulse.Status.UpgradeStartedAt,
		// already cleared above) is what makes this available here.
		upgradeJustCompleted := prevPhase == "Upgrading" && previousUpgradeStartedAt != nil
		if upgradeJustCompleted {
			pulse.Status.LastUpgradeDurationSeconds = int64(time.Since(previousUpgradeStartedAt.Time).Round(time.Second).Seconds())
		}
		if prevPhase != "Running" {
			if upgradeJustCompleted {
				recordEvent(r.Recorder, pulse, corev1.EventTypeNormal, "Running",
					"All components are healthy — upgrade completed in %s",
					time.Duration(pulse.Status.LastUpgradeDurationSeconds)*time.Second)
			} else {
				recordEvent(r.Recorder, pulse, corev1.EventTypeNormal, "Running", "All components are healthy")
			}
		}
	case upgrading:
		pulse.Status.Phase = "Upgrading"
		if prevPhase != "Upgrading" {
			recordEvent(r.Recorder, pulse, corev1.EventTypeNormal, "Upgrading",
				"Detected an in-progress image change: agentImage=%q uiImage=%q", agentImage, uiImage)
		}
	case prevPhase == "Running" || prevPhase == "Upgrading":
		// Was healthy (or healthily mid-upgrade) a moment ago — Degraded
		// rather than Installing, since this is a regression, not a
		// first-time install.
		pulse.Status.Phase = "Degraded"
		if prevPhase != "Degraded" {
			recordEvent(r.Recorder, pulse, corev1.EventTypeWarning, "Degraded",
				"Component health changed: agentHealthy=%v databaseReady=%v uiAvailable=%v",
				agentReady, pgReady, uiReady)
		}
	default:
		pulse.Status.Phase = "Installing"
	}

	setReadyCondition(pulse, "AgentReady", agentReady,
		"agent is healthy", fmt.Sprintf("agent Deployment has 0 ready replicas (image=%s)", agentImage))
	setReadyCondition(pulse, "DatabaseReady", pgReady,
		"database is healthy", "PostgreSQL StatefulSet has 0 ready replicas")
	setReadyCondition(pulse, "UIReady", uiReady,
		"UI is healthy", fmt.Sprintf("UI Deployment has 0 ready replicas (image=%s)", uiImage))

	setComponentReadyGauge(pulse, "agent", agentReady)
	setComponentReadyGauge(pulse, "database", pgReady)
	setComponentReadyGauge(pulse, "ui", uiReady)

	progressing := metav1.Condition{Type: "Progressing", ObservedGeneration: pulse.Generation}
	switch pulse.Status.Phase {
	case "Installing", "Upgrading":
		progressing.Status = metav1.ConditionTrue
		progressing.Reason = pulse.Status.Phase
		progressing.Message = pulse.Status.Phase + " is in progress"
	case "Running":
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = "Stable"
		progressing.Message = "All components are healthy and no rollout is in progress"
	default: // Degraded
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = "Degraded"
		progressing.Message = "Not progressing — a previously healthy component is now unhealthy"
	}
	apimeta.SetStatusCondition(&pulse.Status.Conditions, progressing)

	// Aggregate condition, kept alongside the per-component ones above for
	// backward compatibility with anything already watching "Ready".
	ready := metav1.Condition{Type: "Ready", ObservedGeneration: pulse.Generation}
	if pulse.Status.Phase == "Running" {
		ready.Status = metav1.ConditionTrue
		ready.Reason = "AllComponentsHealthy"
		ready.Message = "Agent, database, and UI are healthy"
	} else {
		ready.Status = metav1.ConditionFalse
		ready.Reason = pulse.Status.Phase
		ready.Message = fmt.Sprintf("agentHealthy=%v databaseReady=%v uiAvailable=%v", agentReady, pgReady, uiReady)
	}
	apimeta.SetStatusCondition(&pulse.Status.Conditions, ready)

	return agentImage, uiImage, agentUpgrading, uiUpgrading
}

// setComponentReadyGauge mirrors a component's ready boolean onto the
// pulse_operator_component_ready metric (see metrics.go) as 1/0.
func setComponentReadyGauge(pulse *pulsev1alpha1.OpenShiftPulse, component string, ready bool) {
	value := 0.0
	if ready {
		value = 1.0
	}
	componentReady.WithLabelValues(pulse.Namespace, pulse.Name, component).Set(value)
}

// setReadyCondition sets a per-component "X is ready" condition, shaped
// consistently across AgentReady/DatabaseReady/UIReady so the three only
// ever differ in Type and their messages.
func setReadyCondition(pulse *pulsev1alpha1.OpenShiftPulse, condType string, ready bool, readyMessage, notReadyMessage string) {
	cond := metav1.Condition{Type: condType, ObservedGeneration: pulse.Generation}
	if ready {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Ready"
		cond.Message = readyMessage
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NotReady"
		cond.Message = notReadyMessage
	}
	apimeta.SetStatusCondition(&pulse.Status.Conditions, cond)
}

// reconcileAutoRollback records a rollback (status.agentRollback /
// status.uiRollback) when an in-progress upgrade (Phase=Upgrading, tracked
// via status.upgradeStartedAt) has not rolled out within
// upgradeHealthTimeout. From then on resolvedImage / resolvedUIImage return
// the last known-healthy image for as long as the CR keeps asking for the
// bad one. Scope is deliberately limited to the agent and UI images — an
// operator-CSV-level rollback would need a separate pre-upgrade gate outside
// this binary, since the operator cannot roll back its own running version
// from inside itself.
//
// Anti-loop guard: one rollback per bad image. While a record exists the
// component runs the last healthy image, so it is no longer upgrading; if
// that image itself fails, the component shows Degraded rather than
// triggering another rollback. Returns true when it recorded one; the caller
// writes status and requeues so the next pass deploys the healthy image.
func (r *OpenShiftPulseReconciler) reconcileAutoRollback(
	ctx context.Context,
	pulse *pulsev1alpha1.OpenShiftPulse,
	agentUpgrading, uiUpgrading bool,
) (bool, error) {
	if pulse.Status.Phase != "Upgrading" || pulse.Status.UpgradeStartedAt == nil {
		return false, nil
	}
	if time.Since(pulse.Status.UpgradeStartedAt.Time) < upgradeHealthTimeout {
		return false, nil
	}

	rolledBack := false
	now := metav1.Now()

	// An agent image held back by spec.agent.minOperatorVersion never ran, so
	// it has not failed; the compatibility condition already says why.
	gated := apimeta.IsStatusConditionFalse(pulse.Status.Conditions, "AgentVersionCompatible")
	if agentUpgrading && !gated && pulse.Status.AgentRollback == nil && pulse.Status.LastHealthyAgentImage != "" {
		badImage := requestedImage(pulse)
		pulse.Status.AgentRollback = &pulsev1alpha1.ImageRollback{From: badImage, To: pulse.Status.LastHealthyAgentImage, At: now}
		recordEvent(r.Recorder, pulse, corev1.EventTypeWarning, "AutoRolledBack",
			"Agent image %q did not become healthy within %s — running last known-healthy %q instead. spec.agent.image is unchanged; set it to another image to try again.",
			badImage, upgradeHealthTimeout, pulse.Status.LastHealthyAgentImage)
		rolledBack = true
	}
	if uiUpgrading && pulse.Status.UIRollback == nil && pulse.Status.LastHealthyUIImage != "" {
		badImage := requestedUIImage(pulse)
		pulse.Status.UIRollback = &pulsev1alpha1.ImageRollback{From: badImage, To: pulse.Status.LastHealthyUIImage, At: now}
		recordEvent(r.Recorder, pulse, corev1.EventTypeWarning, "AutoRolledBack",
			"UI image %q did not become healthy within %s — running last known-healthy %q instead. spec.ui.image is unchanged; set it to another image to try again.",
			badImage, upgradeHealthTimeout, pulse.Status.LastHealthyUIImage)
		rolledBack = true
	}

	if !rolledBack {
		return false, nil
	}
	// The rollback lives in status, not spec: the caller writes status and
	// requeues, and the next pass deploys resolvedImage, which now returns
	// the last healthy image. Writing the spec pinned an operator-defaulted
	// image into the CR for good, and a GitOps tool re-applying the bad image
	// made it roll back again every five minutes.
	pulse.Status.UpgradeStartedAt = nil
	return true, nil
}

// reportPodProblems puts the reason a not-ready agent or UI is not ready into
// its AgentReady/UIReady condition (reason and message from the pod, e.g.
// "ImagePullBackOff") and a Warning PodNotHealthy event. Identical events are
// aggregated by the event recorder, so a pod stuck for an hour is one event
// with a count, not one every reconcile. Nothing is deleted.
func (r *OpenShiftPulseReconciler) reportPodProblems(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, agentReady bool) {
	components := []struct {
		condType, component, deployment string
		ready                           bool
	}{
		{"AgentReady", "agent", agentResourceName(pulse.Name), agentReady},
		{"UIReady", "ui", uiResourceName(pulse.Name), pulse.Status.UIAvailable},
	}
	for _, c := range components {
		if c.ready {
			continue
		}
		problem, err := diagnoseDeploymentPods(ctx, r.Client, pulse.Namespace, c.deployment)
		if err != nil {
			log.FromContext(ctx).Error(err, "listing pods to diagnose", "component", c.component)
			continue
		}
		if problem == nil {
			continue
		}
		apimeta.SetStatusCondition(&pulse.Status.Conditions, metav1.Condition{
			Type:               c.condType,
			Status:             metav1.ConditionFalse,
			Reason:             problem.Reason,
			Message:            problem.String(),
			ObservedGeneration: pulse.Generation,
		})
		recordEvent(r.Recorder, pulse, corev1.EventTypeWarning, "PodNotHealthy", "%s %s", c.component, problem.String())
	}
}
