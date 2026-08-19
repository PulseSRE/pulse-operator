package controller

// Item 3 of the autonomy roadmap: SAFE, advisory-only resource-request
// auto-tuning. agentResources/uiResources (agent_reconciler.go/
// ui_reconciler.go) return static memory defaults regardless of observed
// usage, and nothing calls the metrics.k8s.io API the agent's own
// ClusterRole already grants get/list on (reconcileClusterRole). This file
// closes that gap by reading real metrics.k8s.io PodMetrics for the
// agent/UI pods and surfacing observed-vs-requested memory as Prometheus
// gauges (metrics.go) for dashboard comparison.
//
// Deliberately does NOT patch resources.requests automatically — that is
// an explicit stretch goal the task calls out as out of scope for this
// pass (hysteresis/safety are real, unsolved problems for that). This is
// observability only.
//
// Uses an unstructured client.Client call (no new Go dependency) rather
// than k8s.io/metrics' typed clientset: this operator already talks to
// other GVKs outside its compile-time scheme the same way (Route,
// OAuthClient — see ui_reconciler.go), and controller-runtime's client
// resolves unstructured GVKs via the API server's discovery/RESTMapper
// without needing the type registered anywhere. That is the smaller,
// already-proven-in-this-codebase option, so no new dependency was added.
//
// envtest does not run metrics-server, so metrics.k8s.io is never actually
// served in the test suite (same situation DetectClusterInfo's ACM/
// oauth-proxy checks in cluster_detect.go are already written to handle) —
// every List call below fails closed (logged at V(1), nothing published)
// rather than erroring the reconcile or fabricating a value. The parsing
// logic itself (maxPodMemoryBytes/podMemoryBytes) is a pure function over
// already-fetched unstructured data, so it's unit-tested directly against
// hand-built PodMetrics-shaped fixtures instead.

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var podMetricsListGVK = schema.GroupVersionKind{
	Group:   "metrics.k8s.io",
	Version: "v1beta1",
	Kind:    "PodMetricsList",
}

// observedMemoryCheckInterval bounds how often reconcileObservedMemoryMetrics
// actually calls metrics.k8s.io per (namespace, name, component) — the root
// reconciler's own steady-state cadence is already every 30s
// (OpenShiftPulseReconciler.Reconcile's RequeueAfter), and hitting the
// metrics API on every single one of those, forever, for every managed
// instance, is unnecessary load for a value that is purely advisory and
// does not need second-to-second freshness.
const observedMemoryCheckInterval = 5 * time.Minute

var (
	lastMemCheckMu sync.Mutex
	lastMemCheckAt = map[string]time.Time{} // keyed by "namespace/name/component"
)

// dueForObservedMemoryCheck reports whether enough time has passed since
// the last check for this key to run another one, and — if so — marks now
// as the new last-checked time. now is a parameter (not time.Now()
// internally) so the throttle itself is unit-testable without a real
// sleep.
func dueForObservedMemoryCheck(key string, now time.Time) bool {
	lastMemCheckMu.Lock()
	defer lastMemCheckMu.Unlock()
	if last, ok := lastMemCheckAt[key]; ok && now.Sub(last) < observedMemoryCheckInterval {
		return false
	}
	lastMemCheckAt[key] = now
	return true
}

// podMemoryBytes sums the memory usage across every container in one
// PodMetrics unstructured object (as returned by a metrics.k8s.io/v1beta1
// PodMetricsList), and reports whether at least one container had a
// parseable usage.memory value.
func podMemoryBytes(item unstructured.Unstructured) (int64, bool) {
	containers, found, _ := unstructured.NestedSlice(item.Object, "containers")
	if !found {
		return 0, false
	}
	var total int64
	var any bool
	for _, c := range containers {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		memStr, found, _ := unstructured.NestedString(cm, "usage", "memory")
		if !found {
			continue
		}
		qty, err := resource.ParseQuantity(memStr)
		if err != nil {
			continue
		}
		total += qty.Value()
		any = true
	}
	return total, any
}

// maxPodMemoryBytes reduces a PodMetricsList's items down to the single
// highest per-pod total. Max (not sum or average) is the useful figure
// here: resources.requests.memory is a per-pod value, so the pod closest
// to it (or over it) is what actually matters for "is this default too
// small" — averaging across replicas would hide exactly that pod behind
// healthier ones, and summing conflates a per-replica limit with a
// fleet-wide total that was never the thing being compared.
func maxPodMemoryBytes(items []unstructured.Unstructured) (int64, bool) {
	var max int64
	var any bool
	for _, item := range items {
		bytes, ok := podMemoryBytes(item)
		if !ok {
			continue
		}
		any = true
		if bytes > max {
			max = bytes
		}
	}
	return max, any
}

// reconcileObservedMemoryMetrics sets requestedMemoryBytes unconditionally
// (cheap — no API call), then, at most once per observedMemoryCheckInterval
// per component, queries metrics.k8s.io for the real observed memory usage
// of deploymentName's pods and sets observedMemoryBytes from it. Any
// failure to query or parse (metrics-server not installed, transient API
// error, no samples yet) is logged at V(1) and otherwise a silent no-op —
// this must never fabricate or estimate a value, per this repo's real-data
// convention.
func reconcileObservedMemoryMetrics(
	ctx context.Context,
	c client.Client,
	pulse *pulsev1alpha1.OpenShiftPulse,
	deploymentName, component string,
	requestedBytes int64,
) {
	requestedMemoryBytes.WithLabelValues(pulse.Namespace, pulse.Name, component).Set(float64(requestedBytes))

	key := pulse.Namespace + "/" + pulse.Name + "/" + component
	now := time.Now()
	if !dueForObservedMemoryCheck(key, now) {
		return
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(podMetricsListGVK)
	if err := c.List(ctx, list,
		client.InNamespace(pulse.Namespace),
		client.MatchingLabels{"app": deploymentName},
	); err != nil {
		log.FromContext(ctx).V(1).Info("metrics.k8s.io PodMetrics unavailable — skipping observed-memory sample",
			"component", component, "deployment", deploymentName, "error", err.Error())
		return
	}

	maxBytes, any := maxPodMemoryBytes(list.Items)
	if !any {
		return // no parseable samples (e.g. pods not Running yet) — publish nothing rather than a bogus 0.
	}
	observedMemoryBytes.WithLabelValues(pulse.Namespace, pulse.Name, component).Set(float64(maxBytes))
}

// agentRequestedMemoryBytes and uiRequestedMemoryBytes read the effective
// resources.requests.memory already resolved by agentResources/uiResources
// (agent_reconciler.go/ui_reconciler.go) — reused here rather than
// re-deriving the spec-override-or-default logic a second time.
func agentRequestedMemoryBytes(cr *pulsev1alpha1.OpenShiftPulse) int64 {
	qty := agentResources(cr).Requests[corev1.ResourceMemory]
	return qty.Value()
}

func uiRequestedMemoryBytes(cr *pulsev1alpha1.OpenShiftPulse) int64 {
	qty := uiResources(cr).Requests[corev1.ResourceMemory]
	return qty.Value()
}

// resetObservedMemoryCheckCache clears the throttle cache — for testing
// only (mirrors ResetClusterInfoCache in cluster_detect.go), so one spec's
// calls can't make a later spec's assertions about dueForObservedMemoryCheck
// depend on suite run order.
func resetObservedMemoryCheckCache() {
	lastMemCheckMu.Lock()
	defer lastMemCheckMu.Unlock()
	lastMemCheckAt = map[string]time.Time{}
}
