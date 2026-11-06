package controller

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var (
	pinnedRunner    = "quay.io/amobrem/openshiftpulse-helm-runner@sha256:" + strings.Repeat("a", 64)
	pinnedRunnerEnv = "mirror.example.com/pulse/openshiftpulse-helm-runner@sha256:" + strings.Repeat("b", 64)
)

// configJSRe extracts the body nginx returns for /config.js.
var configJSRe = regexp.MustCompile(`location = /config\.js \{\n\s+add_header Content-Type application/javascript;\n\s+return 200 '([^']*)';`)

// configJSBodies returns every /config.js body in an nginx.conf (one per
// server block that serves the SPA).
func configJSBodies(conf string) []string {
	var out []string
	for _, m := range configJSRe.FindAllStringSubmatch(conf, -1) {
		out = append(out, m[1])
	}
	return out
}

// parseConfigJS checks the body assigns the global the UI reads and returns
// the assigned object.
func parseConfigJS(body string) map[string]any {
	GinkgoHelper()
	prefix := "window.__OPENSHIFTPULSE_CONFIG__="
	Expect(body).To(HavePrefix(prefix), "config.js must assign the global pulse-ui reads (src/kubeview/config/runtime.ts)")
	Expect(body).To(HaveSuffix(";"))
	var cfg map[string]any
	Expect(json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(body, prefix), ";")), &cfg)).To(Succeed())
	return cfg
}

func runnerImageIn(cfg map[string]any) (string, bool) {
	hi, ok := cfg["helmInstall"].(map[string]any)
	if !ok {
		return "", false
	}
	img, ok := hi["runnerImage"].(string)
	return img, ok
}

// unsetHelmRunnerEnv clears RELATED_IMAGE_HELM_RUNNER for one spec and
// restores it afterwards.
func unsetHelmRunnerEnv() {
	prev, had := os.LookupEnv(envHelmRunnerImage)
	Expect(os.Unsetenv(envHelmRunnerImage)).To(Succeed())
	DeferCleanup(func() {
		if had {
			_ = os.Setenv(envHelmRunnerImage, prev)
		} else {
			_ = os.Unsetenv(envHelmRunnerImage)
		}
	})
}

func setHelmRunnerEnv(v string) {
	unsetHelmRunnerEnv()
	Expect(os.Setenv(envHelmRunnerImage, v)).To(Succeed())
}

var _ = Describe("UIReconciler /config.js", func() {
	const (
		crName    = "configjs-test-pulse"
		namespace = "default"
	)

	var (
		cr *pulsev1alpha1.OpenShiftPulse
		ui *UIReconciler
	)

	BeforeEach(func() {
		ui = &UIReconciler{Client: k8sClient, Scheme: testScheme}
		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		unsetHelmRunnerEnv()
	})

	create := func() {
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(testCtx, cr)
			_ = k8sClient.Delete(testCtx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: uiNginxSecretName(crName), Namespace: namespace}})
		})
	}

	condition := func() *metav1.Condition {
		return apimeta.FindStatusCondition(cr.Status.Conditions, helmRunnerConditionType)
	}

	It("assigns window.__OPENSHIFTPULSE_CONFIG__, never the unread __PULSE_CONFIG__", func() {
		create()
		conf := reconcileNginxAndRead(testCtx, ui, cr)
		Expect(conf).NotTo(ContainSubstring("__PULSE_CONFIG__"))
		bodies := configJSBodies(conf)
		Expect(bodies).To(HaveLen(1))
		parseConfigJS(bodies[0])
	})

	It("unset: serves an empty object and reports Helm as not configured", func() {
		create()
		bodies := configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))
		Expect(bodies).To(Equal([]string{"window.__OPENSHIFTPULSE_CONFIG__={};"}))

		c := condition()
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("NotSet"))
		Expect(c.Message).To(ContainSubstring("Helm actions in the UI are disabled"))
	})

	It("set in the CR: serves helmInstall.runnerImage", func() {
		cr.Spec.UI.HelmRunnerImage = pinnedRunner
		create()
		bodies := configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))
		Expect(bodies).To(HaveLen(1))
		img, ok := runnerImageIn(parseConfigJS(bodies[0]))
		Expect(ok).To(BeTrue())
		Expect(img).To(Equal(pinnedRunner))
		Expect(bodies[0]).To(Equal(`window.__OPENSHIFTPULSE_CONFIG__={"helmInstall":{"runnerImage":"` + pinnedRunner + `"}};`))

		c := condition()
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("Pinned"))
		Expect(c.Message).To(ContainSubstring("spec.ui.helmRunnerImage"))
	})

	It("defaults from RELATED_IMAGE_HELM_RUNNER, and the CR wins over it", func() {
		setHelmRunnerEnv(pinnedRunnerEnv)
		create()
		img, ok := runnerImageIn(parseConfigJS(configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))[0]))
		Expect(ok).To(BeTrue())
		Expect(img).To(Equal(pinnedRunnerEnv))
		Expect(condition().Message).To(ContainSubstring(envHelmRunnerImage))

		cr.Spec.UI.HelmRunnerImage = pinnedRunner
		img, _ = runnerImageIn(parseConfigJS(configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))[0]))
		Expect(img).To(Equal(pinnedRunner))
	})

	It("rejects a tag reference in the CR: serves nothing, does not fall back to the release pin, says why", func() {
		setHelmRunnerEnv(pinnedRunnerEnv)
		cr.Spec.UI.HelmRunnerImage = "quay.io/amobrem/openshiftpulse-helm-runner:v2.38.0"
		create()
		bodies := configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))
		Expect(bodies).To(Equal([]string{"window.__OPENSHIFTPULSE_CONFIG__={};"}))

		c := condition()
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("NotDigestPinned"))
		Expect(c.Message).To(ContainSubstring("spec.ui.helmRunnerImage"))
		Expect(c.Message).To(ContainSubstring("openshiftpulse-helm-runner:v2.38.0"))
	})

	It("rejects a non-digest RELATED_IMAGE_HELM_RUNNER the same way", func() {
		setHelmRunnerEnv("quay.io/amobrem/openshiftpulse-helm-runner:latest")
		create()
		Expect(configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))).To(Equal([]string{"window.__OPENSHIFTPULSE_CONFIG__={};"}))
		Expect(condition().Reason).To(Equal("NotDigestPinned"))
		Expect(condition().Message).To(ContainSubstring(envHelmRunnerImage))
	})

	It("serves the same config.js on the render listener", func() {
		enabled := true
		cr.Spec.Render.Enabled = &enabled
		cr.Spec.UI.HelmRunnerImage = pinnedRunner
		create()
		bodies := configJSBodies(reconcileNginxAndRead(testCtx, ui, cr))
		Expect(bodies).To(HaveLen(2))
		Expect(bodies[0]).To(Equal(bodies[1]))
	})

	It("rolls the UI when the runner image changes (nginx config hash moves)", func() {
		create()
		before, err := ui.reconcileUINginxConfigMap(testCtx, cr)
		Expect(err).NotTo(HaveOccurred())
		cr.Spec.UI.HelmRunnerImage = pinnedRunner
		after, err := ui.reconcileUINginxConfigMap(testCtx, cr)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
	})
})

// TestHelmRunnerImagePattern pins what counts as digest-pinned, and that
// nothing which could break out of the single-quoted nginx string or the
// JavaScript it is served in is accepted.
func TestHelmRunnerImagePattern(t *testing.T) {
	hex := strings.Repeat("0123456789abcdef", 4)
	for _, ok := range []string{
		"quay.io/amobrem/openshiftpulse-helm-runner@sha256:" + hex,
		"quay.io/amobrem/openshiftpulse-helm-runner:v2.38.0@sha256:" + hex,
		"registry.example.com:5000/a/b_c/d.e@sha256:" + hex,
	} {
		if !helmRunnerImagePattern.MatchString(ok) {
			t.Errorf("rejected digest-pinned %q", ok)
		}
	}
	for _, bad := range []string{
		"",
		"quay.io/amobrem/openshiftpulse-helm-runner",
		"quay.io/amobrem/openshiftpulse-helm-runner:latest",
		"quay.io/amobrem/openshiftpulse-helm-runner@sha256:" + hex[:63],
		"quay.io/amobrem/openshiftpulse-helm-runner@sha256:" + strings.ToUpper(hex),
		"quay.io/amobrem/openshiftpulse-helm-runner@sha512:" + hex + hex,
		"@sha256:" + hex,
		" quay.io/x@sha256:" + hex,
		"quay.io/x@sha256:" + hex + "\n",
		"quay.io/x';return 200 'x@sha256:" + hex,
		"quay.io/$host/x@sha256:" + hex,
		`quay.io/x\"}@sha256:` + hex,
		"quay.io/x</script>@sha256:" + hex,
		"quay.io/a@b@sha256:" + hex,
	} {
		if helmRunnerImagePattern.MatchString(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestUIRuntimeConfigJS(t *testing.T) {
	if got := uiRuntimeConfigJS(helmRunnerSetting{}); got != "window.__OPENSHIFTPULSE_CONFIG__={};" {
		t.Errorf("unset: got %q", got)
	}
	got := uiRuntimeConfigJS(helmRunnerSetting{image: pinnedRunner})
	want := `window.__OPENSHIFTPULSE_CONFIG__={"helmInstall":{"runnerImage":"` + pinnedRunner + `"}};`
	if got != want {
		t.Errorf("set: got %q, want %q", got, want)
	}
}
