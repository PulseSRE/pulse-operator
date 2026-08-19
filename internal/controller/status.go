package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	agentReady, pgReady bool,
) (agentImage, uiImage string, agentUpgrading, uiUpgrading bool) {
	uiReady := pulse.Status.UIAvailable
	agentImage = resolvedImage(pulse)
	uiImage = resolvedUIImage(pulse)

	agentUpgrading = !agentReady && pulse.Status.LastHealthyAgentImage != "" &&
		agentImage != pulse.Status.LastHealthyAgentImage
	uiUpgrading = !uiReady && pulse.Status.LastHealthyUIImage != "" &&
		uiImage != pulse.Status.LastHealthyUIImage

	// Only record an image as "known healthy" once its component is
	// actually ready with it — this is what makes the comparison above mean
	// "differs from the last image that worked", not just "differs from
	// whatever was there before".
	if agentReady && agentImage != "" {
		pulse.Status.LastHealthyAgentImage = agentImage
	}
	if uiReady && uiImage != "" {
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
	case allReady:
		pulse.Status.Phase = "Running"
		if prevPhase != "Running" {
			recordEvent(r.Recorder, pulse, corev1.EventTypeNormal, "Running", "All components are healthy")
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

// reconcileAutoRollback reverts spec.agent.image and/or spec.ui.image back
// to the last known-healthy image when an in-progress upgrade
// (Phase=Upgrading, tracked via status.upgradeStartedAt) has not become
// healthy within upgradeHealthTimeout. Scope is deliberately limited to the
// agent and UI images — an operator-CSV-level rollback would need a
// separate pre-upgrade gate outside this binary, since the operator cannot
// roll back its own running version from inside itself.
//
// Anti-loop guard: only patches when spec still differs from lastHealthy.
// If spec already equals lastHealthy (e.g. this function's own previous
// patch already landed, or an admin already reverted it by hand) there is
// nothing left to roll back — a rollback target that itself never becomes
// healthy surfaces as Degraded on the next syncPhaseAndConditions call
// instead of this function retriggering the same patch forever.
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

	if agentUpgrading && pulse.Spec.Agent.Image != pulse.Status.LastHealthyAgentImage {
		badImage := pulse.Spec.Agent.Image
		pulse.Spec.Agent.Image = pulse.Status.LastHealthyAgentImage
		recordEvent(r.Recorder, pulse, corev1.EventTypeWarning, "AutoRolledBack",
			"Agent image %q did not become healthy within %s — reverted spec.agent.image to last known-healthy %q",
			badImage, upgradeHealthTimeout, pulse.Status.LastHealthyAgentImage)
		rolledBack = true
	}
	if uiUpgrading && pulse.Spec.UI.Image != pulse.Status.LastHealthyUIImage {
		badImage := pulse.Spec.UI.Image
		pulse.Spec.UI.Image = pulse.Status.LastHealthyUIImage
		recordEvent(r.Recorder, pulse, corev1.EventTypeWarning, "AutoRolledBack",
			"UI image %q did not become healthy within %s — reverted spec.ui.image to last known-healthy %q",
			badImage, upgradeHealthTimeout, pulse.Status.LastHealthyUIImage)
		rolledBack = true
	}

	if !rolledBack {
		return false, nil
	}
	if err := r.Update(ctx, pulse); err != nil {
		return false, fmt.Errorf("auto-rollback spec update: %w", err)
	}
	return true, nil
}
