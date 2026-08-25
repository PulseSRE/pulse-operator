// Agent/UI image skew detection.
//
// The agent and the UI are released as a coordinated pair carrying one version
// number: pulse-agent v2.26.1 ships alongside pulse-ui v2.26.1. Nothing
// enforced that on a cluster, though, because the operator faithfully
// reconciles whatever spec.agent.image and spec.ui.image say and has no opinion
// about whether they agree.
//
// The failure that motivated this: dev05 ran spec.agent.image v2.25.0 with
// spec.ui.image v2.24.0 for weeks. Both Deployments were healthy, every CI
// pipeline was green, the CR reported Ready, and the two surfaces reported
// different versions to users. Nothing anywhere was wrong enough to notice.
//
// This is deliberately a *comparison between the two pinned tags*, not a check
// against the newest published release. Asking "is this the latest?" would put
// a registry or GitHub call in the reconcile path, make the answer depend on
// network reachability, and flag every cluster that has legitimately chosen to
// stay on an older version. Asking "do these two agree?" needs no network, is
// deterministic, and encodes the actual project invariant.
//
// It reports; it does not block. A rollout patches the two images at slightly
// different moments, so a transient mismatch is normal and resolves itself —
// blocking would wedge the very upgrade that clears the condition.
package controller

import (
	"fmt"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// imageTag returns the tag portion of a container image reference, or "" when
// the reference carries no tag, is digest-pinned, or is empty.
//
// Splitting on the last colon is not enough on its own: a registry may include
// a port ("registry.local:5000/pulse-agent"), and that colon must not be read
// as a tag separator. Only a colon appearing after the final "/" is a tag.
func imageTag(image string) string {
	if image == "" {
		return ""
	}
	// Digest-pinned references have no meaningful version tag to compare.
	if strings.Contains(image, "@") {
		return ""
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash {
		return ""
	}
	return image[lastColon+1:]
}

// agentUIVersionSkew reports whether the CR pins agent and UI images at
// different versions, and an explanatory message when it does.
//
// Fails open in every ambiguous case — an unset image (the operator supplies a
// default), a digest pin, or a floating tag like "latest" all report no skew
// rather than guessing. A false alarm on a deliberately-unusual pin is worse
// than staying quiet, because a condition nobody trusts is a condition nobody
// reads.
func agentUIVersionSkew(cr *pulsev1alpha1.OpenShiftPulse) (skewed bool, message string) {
	agentTag := imageTag(cr.Spec.Agent.Image)
	uiTag := imageTag(cr.Spec.UI.Image)

	if agentTag == "" || uiTag == "" {
		return false, ""
	}
	// A floating tag says nothing about which version is actually running.
	if agentTag == "latest" || uiTag == "latest" {
		return false, ""
	}
	if agentTag == uiTag {
		return false, ""
	}
	return true, fmt.Sprintf(
		"spec.agent.image is pinned at %s but spec.ui.image is pinned at %s. "+
			"The agent and UI are released together under one version; a mismatch "+
			"means one surface was upgraded and the other was not, and the two will "+
			"report different versions to users. This is expected only briefly during "+
			"an upgrade.",
		agentTag, uiTag,
	)
}

// setVersionSkewCondition records the agent/UI skew verdict on the CR.
//
// True means the two agree (the healthy state), so that a cluster in good
// standing reads as VersionSkew=True alongside its other positive conditions
// rather than inverting the polarity of the status block.
func setVersionSkewCondition(cr *pulsev1alpha1.OpenShiftPulse, skewed bool, message string) {
	cond := metav1.Condition{Type: "AgentUIVersionsMatch", ObservedGeneration: cr.Generation}
	if skewed {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "VersionSkew"
		cond.Message = message
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "VersionsMatch"
		cond.Message = "spec.agent.image and spec.ui.image are pinned at the same version"
	}
	apimeta.SetStatusCondition(&cr.Status.Conditions, cond)
}
