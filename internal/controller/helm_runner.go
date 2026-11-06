package controller

// The Helm runner image and the UI's runtime config (/config.js).
//
// pulse-ui's Helm actions (install, upgrade, rollback, uninstall) run as Jobs
// that the UI creates as the signed-in user, together with their
// ServiceAccount, Role and RoleBinding. The operator deploys none of that and
// needs no RBAC for it. What the UI cannot know on its own is which runner
// image to use: it reads window.__OPENSHIFTPULSE_CONFIG__.helmInstall.runnerImage
// (pulse-ui src/kubeview/config/runtime.ts) and refuses any image that is not
// digest-pinned (helmInstall.ts). The operator serves that value from the
// nginx /config.js location it already owns.
//
// Before this, /config.js served window.__PULSE_CONFIG__={}, a global nothing
// in the UI reads, so on an operator install every Helm Job was refused.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// uiRuntimeConfigGlobal is the global the UI reads its runtime config from.
const uiRuntimeConfigGlobal = "window.__OPENSHIFTPULSE_CONFIG__"

// helmRunnerConditionType reports whether the UI was given a Helm runner image.
const helmRunnerConditionType = "HelmRunnerImageConfigured"

// helmRunnerImagePattern accepts <repository>[:tag]@sha256:<64 lowercase hex>.
//
// It is stricter than the UI's own check (/^[^\s@]+@sha256:[a-f0-9]{64}$/i),
// so anything accepted here is accepted there. The narrow character set is
// also what makes the value safe to embed: it is written into a single-quoted
// nginx string (where ' ends the string, \ escapes and $ interpolates a
// variable) and into JavaScript, and none of those characters can match.
var helmRunnerImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[a-f0-9]{64}$`)

// helmRunnerSetting is the resolved Helm runner image and the condition that
// explains it. image is empty unless it may be served.
type helmRunnerSetting struct {
	image   string
	status  metav1.ConditionStatus
	reason  string
	message string
}

// resolveHelmRunner picks the Helm runner image: spec.ui.helmRunnerImage,
// else RELATED_IMAGE_HELM_RUNNER. There is no compiled default. A CR value
// that is not digest-pinned is reported, not replaced by the release pin: the
// author asked for that image, and silently running another would hide it.
func resolveHelmRunner(cr *pulsev1alpha1.OpenShiftPulse) helmRunnerSetting {
	image, source := cr.Spec.UI.HelmRunnerImage, "spec.ui.helmRunnerImage"
	if image == "" {
		image, source = os.Getenv(envHelmRunnerImage), envHelmRunnerImage
	}
	switch {
	case image == "":
		return helmRunnerSetting{
			status: metav1.ConditionFalse,
			reason: "NotSet",
			message: "No Helm runner image: spec.ui.helmRunnerImage is unset and this operator release pins none (" +
				envHelmRunnerImage + "). Helm actions in the UI are disabled.",
		}
	case !helmRunnerImagePattern.MatchString(image):
		return helmRunnerSetting{
			status: metav1.ConditionFalse,
			reason: "NotDigestPinned",
			message: fmt.Sprintf("%s is %q, which is not digest-pinned (<repository>@sha256:<64 hex>). "+
				"The UI refuses a runner image that is not pinned by digest, so none is served and Helm actions in the UI are disabled.",
				source, truncate(image, 256)),
		}
	default:
		return helmRunnerSetting{
			image:   image,
			status:  metav1.ConditionTrue,
			reason:  "Pinned",
			message: fmt.Sprintf("The UI runs Helm Jobs with %s (from %s)", image, source),
		}
	}
}

// setHelmRunnerCondition records the Helm runner verdict on the CR. The caller
// writes status.
func setHelmRunnerCondition(cr *pulsev1alpha1.OpenShiftPulse, s helmRunnerSetting) {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               helmRunnerConditionType,
		Status:             s.status,
		Reason:             s.reason,
		Message:            s.message,
		ObservedGeneration: cr.Generation,
	})
}

type uiHelmInstallConfig struct {
	RunnerImage string `json:"runnerImage,omitempty"`
}

type uiRuntimeConfig struct {
	HelmInstall *uiHelmInstallConfig `json:"helmInstall,omitempty"`
}

// uiRuntimeConfigJS is the body of /config.js. With no runner image it is an
// empty object, and the UI refuses Helm actions with its existing "Helm runner
// image must be digest-pinned" error.
// helmInstall.serviceAccountName is left to the UI's default.
func uiRuntimeConfigJS(s helmRunnerSetting) string {
	cfg := uiRuntimeConfig{}
	if s.image != "" {
		cfg.HelmInstall = &uiHelmInstallConfig{RunnerImage: s.image}
	}
	raw, err := json.Marshal(cfg)
	if err != nil { // cannot happen for this type
		raw = []byte("{}")
	}
	return uiRuntimeConfigGlobal + "=" + string(raw) + ";"
}
