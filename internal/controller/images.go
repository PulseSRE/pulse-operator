package controller

// Central resolution for every image this operator deploys.
//
// Each component image is resolved in three steps, highest precedence first:
//
//  1. The CR's own override (spec.agent.image, spec.ui.image, ...). An author
//     who pins an image means it.
//  2. A RELATED_IMAGE_* environment variable on the manager container. OLM
//     sets these from the CSV at install time, which is how a release pins the
//     exact digests it was tested against — and how a disconnected cluster
//     gets mirrored references instead of quay.io ones.
//  3. The compiled-in default below. Only reached by `make run` and tests;
//     a bundle install always has the env set.
//
// Why this exists at all: the compiled defaults for the four first-party
// images were mutable tags (`:latest`, and `:mcp-server`). Kubernetes only
// defaults imagePullPolicy to Always when the tag is literally "latest", so
// `pulse-agent:mcp-server` got IfNotPresent — mutable like a floating tag, but
// cached like a pinned one. A pod could therefore run for months on an image
// that no longer existed under that tag, then silently pull a different,
// incompatible build the first time it was rescheduled onto a node without a
// cached copy. That is not hypothetical: it took the MCP server down when the
// published `mcp-server` image changed its CLI (dropping --port) while the
// operator kept passing the old flags.
//
// temporal_reconciler.go already pinned its images for the same reason, with
// the same reasoning spelled out. This brings the first-party images in line.

import "os"

// RELATED_IMAGE_* names. OLM convention: a CSV's spec.relatedImages entries
// paired with identically-valued env vars on the manager container is what
// makes `opm render` / `oc adm catalog mirror` able to find every image an
// operator will pull, not just the operator's own.
const (
	envAgentImage      = "RELATED_IMAGE_AGENT"
	envUIImage         = "RELATED_IMAGE_UI"
	envMCPServerImage  = "RELATED_IMAGE_MCP_SERVER"
	envRenderImage     = "RELATED_IMAGE_RENDER"
	envPostgreSQLImage = "RELATED_IMAGE_POSTGRESQL"
	envOAuthProxyImage = "RELATED_IMAGE_OAUTH_PROXY"
	envTemporalImage   = "RELATED_IMAGE_TEMPORAL"
	envTemporalUIImage = "RELATED_IMAGE_TEMPORAL_UI"
)

// imageFromEnv returns the operand image pinned by the bundle, falling back to
// the compiled default when the variable is unset or empty. Empty is treated as
// unset deliberately: a CSV that ships `value: ""` for an image it forgot to
// template should degrade to the known-good default rather than produce a
// Deployment with an empty image field, which the API server rejects with an
// error that points nowhere near the real cause.
func imageFromEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
