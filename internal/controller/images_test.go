package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestImageFromEnvPrecedence(t *testing.T) {
	const fallback = "quay.io/example/thing:v1"

	t.Run("unset falls back", func(t *testing.T) {
		t.Setenv(envAgentImage, "")
		_ = os.Unsetenv(envAgentImage)
		if got := imageFromEnv(envAgentImage, fallback); got != fallback {
			t.Fatalf("unset env: got %q, want %q", got, fallback)
		}
	})

	// An image field the API server rejects ("" is not a valid image) is a far
	// worse failure than quietly using the known-good default, and the error it
	// produces points at the Deployment rather than at the CSV that forgot to
	// template the value.
	t.Run("empty falls back", func(t *testing.T) {
		t.Setenv(envAgentImage, "")
		if got := imageFromEnv(envAgentImage, fallback); got != fallback {
			t.Fatalf("empty env: got %q, want %q", got, fallback)
		}
	})

	t.Run("set wins", func(t *testing.T) {
		pinned := "quay.io/example/thing@sha256:" + strings.Repeat("a", 64)
		t.Setenv(envAgentImage, pinned)
		if got := imageFromEnv(envAgentImage, fallback); got != pinned {
			t.Fatalf("set env: got %q, want %q", got, pinned)
		}
	})
}

// csvOperandImageEnvs is every RELATED_IMAGE_* this operator reads. Adding a
// new operand means adding it here, which is what makes the two tests below
// fail until the CSV is updated to match.
var csvOperandImageEnvs = []string{
	envAgentImage,
	envUIImage,
	envMCPServerImage,
	envRenderImage,
	envPostgreSQLImage,
	envOAuthProxyImage,
	envTemporalImage,
	envTemporalUIImage,
}

type csvDoc struct {
	Spec struct {
		RelatedImages []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"relatedImages"`
		Install struct {
			Spec struct {
				Deployments []struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Name string `json:"name"`
									Env  []struct {
										Name  string `json:"name"`
										Value string `json:"value"`
									} `json:"env"`
								} `json:"containers"`
							} `json:"spec"`
						} `json:"template"`
					} `json:"spec"`
				} `json:"deployments"`
			} `json:"spec"`
		} `json:"install"`
	} `json:"spec"`
}

func loadCSV(t *testing.T) csvDoc {
	t.Helper()
	path := filepath.Join("..", "..", "bundle", "manifests", "pulse-operator.clusterserviceversion.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CSV: %v", err)
	}
	var doc csvDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	return doc
}

// TestCSVSetsEveryOperandImageEnv is the guard that keeps spec.relatedImages
// honest. Before this, the CSV listed relatedImages that nothing consumed: the
// manager container had no corresponding env vars, so the operator used its
// compiled-in defaults and the CSV's list was documentation that could drift
// from reality without anything noticing. It had already drifted — render and
// the two Temporal images were missing entirely.
//
// relatedImages is not cosmetic. `oc adm catalog mirror` and disconnected
// installs discover an operator's operands from it, so an image missing here
// is an image that will not be mirrored, and a pull that fails on a
// disconnected cluster at the worst possible moment.
func TestCSVSetsEveryOperandImageEnv(t *testing.T) {
	doc := loadCSV(t)

	var manager *struct {
		Name string `json:"name"`
		Env  []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	}
	for i := range doc.Spec.Install.Spec.Deployments {
		cs := doc.Spec.Install.Spec.Deployments[i].Spec.Template.Spec.Containers
		for j := range cs {
			if cs[j].Name == "manager" {
				manager = &cs[j]
			}
		}
	}
	if manager == nil {
		t.Fatal("CSV has no container named manager")
	}

	present := map[string]string{}
	for _, e := range manager.Env {
		present[e.Name] = e.Value
	}
	for _, want := range csvOperandImageEnvs {
		v, ok := present[want]
		if !ok {
			t.Errorf("CSV manager container is missing env %s — the operator will silently use its compiled default instead of the released image", want)
			continue
		}
		if v == "" {
			t.Errorf("CSV env %s is empty", want)
		}
	}

	related := map[string]bool{}
	for _, r := range doc.Spec.RelatedImages {
		related[r.Image] = true
	}
	for _, name := range csvOperandImageEnvs {
		v := present[name]
		if v != "" && !related[v] {
			t.Errorf("env %s=%q has no matching spec.relatedImages entry; it will not be mirrored for disconnected installs", name, v)
		}
	}
}

// TestCSVOperandImagesArePinnedByDigest is the test that would have caught the
// outage this change exists to prevent.
//
// Kubernetes defaults imagePullPolicy to Always only when the tag is literally
// "latest"; every other tag gets IfNotPresent. `pulse-agent:mcp-server` is
// therefore mutable like a floating tag but cached like a pinned one, so a pod
// can run for months on an image that tag no longer points at, then pull a
// different and possibly incompatible build the first time it is rescheduled
// onto a node with no cached copy. That is exactly what happened: the
// published mcp-server image changed its CLI and dropped --port while the
// operator kept passing it, and the server crash-looped the moment a drain
// moved the pod.
//
// A digest cannot change under a running deployment, which makes IfNotPresent
// caching correct rather than dangerous, and makes an operator release
// reproducible: it deploys the operands it was tested against.
func TestCSVOperandImagesArePinnedByDigest(t *testing.T) {
	doc := loadCSV(t)
	if len(doc.Spec.RelatedImages) == 0 {
		t.Fatal("CSV declares no relatedImages")
	}
	for _, r := range doc.Spec.RelatedImages {
		if !strings.Contains(r.Image, "@sha256:") {
			t.Errorf("relatedImages %q is %q — operand images must be digest-pinned, not tag-referenced", r.Name, r.Image)
		}
	}
}
