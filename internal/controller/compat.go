package controller

// Item 2 of the autonomy roadmap: a pre-upgrade compatibility gate for
// spec.agent.image changes.
//
// Investigation (done before writing anything here, per this repo's "never
// use mock data or made-up values" convention): cloned
// github.com/PulseSRE/pulse-agent read-only and looked for a real,
// checkable compatibility signal to gate on.
//
// Found one real signal — pulse-agent exposes `GET /version`, returning
// {"protocol": "2", "agent": "<semver>", "tools": N, "skills": N,
// "features": [...]}, and that repo's API_CONTRACT.md documents a full
// "Protocol Version History" / "Release Compatibility Matrix" for it. But
// it is explicitly scoped to *UI<->Agent* wire-protocol compatibility
// ("Both repos must implement the same protocol version for
// compatibility") and is already checked client-side by the UI itself
// ("The UI sends a GET /version request before connecting. If the agent's
// protocol field doesn't match the UI's EXPECTED_PROTOCOL, the UI shows a
// warning but still connects."). It says nothing about a minimum required
// *operator* version, and nothing else in pulse-agent (no image
// annotation/label, no CRD-version compatibility table) speaks to
// operator<->agent compatibility, which is the axis this item is actually
// about ("an operator upgrade expecting a newer agent API/DB schema but
// pointed at an old agent image, or vice versa"). Reusing the UI/Agent
// protocol signal for a purpose it was never designed for — and that the
// UI already independently handles — would not be honest use of a real
// signal; it would just be a different flavor of guessing.
//
// So this gate takes the second path the task explicitly sanctions for
// exactly this case: spec.agent.minOperatorVersion is a plain, optional
// field. Nothing populates it automatically. Left unset (the default —
// true for every existing CR), agentVersionCompatible always returns
// compatible=true, so the gate is completely inert until an admin
// deliberately opts in. When an admin does set it, it is checked against
// OperatorVersion below — this operator build's own version — with a
// clear, dedicated status condition when it fails (see
// setAgentVersionCompatibleCondition).
import (
	"fmt"

	"github.com/Masterminds/semver/v3"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// OperatorVersion is this operator build's own version, compared against an
// admin-set spec.agent.minOperatorVersion (see this file's doc comment
// above). This mirrors the git tag of the last release (v0.4.0, live on
// cluster at the time this constant was last synced) — the separate release/
// version-bump step that tags future releases is responsible for keeping this
// constant in sync; it is deliberately not touched by anything else in
// this change.
const OperatorVersion = "0.4.0"

// agentVersionCompatible reports whether this operator build satisfies
// cr.Spec.Agent.MinOperatorVersion, and an explanatory message when it does
// not. Empty (unset) is always compatible — see this file's doc comment.
// A malformed version string, on either side, fails OPEN (compatible)
// rather than blocking a real deployment on something as easy to get
// wrong as a typo'd semver string that the CRD does not itself validate;
// silently treating "we couldn't parse this" the same as "incompatible"
// would be exactly the kind of confusing failure mode this item exists to
// remove, not add.
func agentVersionCompatible(cr *pulsev1alpha1.OpenShiftPulse) (compatible bool, message string) {
	want := cr.Spec.Agent.MinOperatorVersion
	if want == "" {
		return true, ""
	}
	wantVer, err := semver.NewVersion(want)
	if err != nil {
		return true, ""
	}
	haveVer, err := semver.NewVersion(OperatorVersion)
	if err != nil {
		return true, ""
	}
	if haveVer.LessThan(wantVer) {
		return false, fmt.Sprintf(
			"spec.agent.minOperatorVersion=%s requires a pulse-operator build >= %s; this operator is running %s",
			want, want, OperatorVersion,
		)
	}
	return true, ""
}

// setAgentVersionCompatibleCondition records agentVersionCompatible's
// outcome as its own condition type, deliberately additive to (never
// touched by) syncPhaseAndConditions' AgentReady/DatabaseReady/UIReady/
// Progressing/Ready conditions. This is a statement about whether a
// *requested* spec change is allowed to be applied, independent of current
// component health — folding it into Progressing (which
// syncPhaseAndConditions fully owns and recomputes unconditionally every
// reconcile) would just get immediately overwritten in the same pass.
func setAgentVersionCompatibleCondition(cr *pulsev1alpha1.OpenShiftPulse, compatible bool, message string) {
	cond := metav1.Condition{Type: "AgentVersionCompatible", ObservedGeneration: cr.Generation}
	if compatible {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Compatible"
		cond.Message = "spec.agent.minOperatorVersion is satisfied by this operator build"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "IncompatibleVersion"
		cond.Message = message
	}
	apimeta.SetStatusCondition(&cr.Status.Conditions, cond)
}
