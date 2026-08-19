package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// selfHealActionsTotal counts self-heal/remediation actions the operator
	// has taken on its own (the same actions recordEvent's "SelfHealed" call
	// sites report as Events) — Events are visible per-instance via `oc get
	// events`, but this is what makes "how often is this actually happening
	// across the fleet" answerable without grepping event logs.
	selfHealActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pulse_operator_self_heal_actions_total",
			Help: "Total number of self-heal/remediation actions the operator has taken, by component and action.",
		},
		[]string{"component", "action"},
	)

	// componentReady mirrors the AgentReady/DatabaseReady/UIReady status
	// conditions as a gauge (1=True, 0=False) per OpenShiftPulse instance,
	// so dashboards/alerts don't need to poll each CR's status subresource
	// directly.
	componentReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pulse_operator_component_ready",
			Help: "Whether a component of an OpenShiftPulse instance is ready (1) or not (0).",
		},
		[]string{"namespace", "name", "component"},
	)

	// reconcileErrorsTotal counts reconcile failures by step.
	// controller-runtime's own generic controller_runtime_reconcile_errors_total
	// metric already exists but is keyed by controller name only (always
	// "openshiftpulse" here, since every sub-reconciler in this operator is
	// called in-process rather than registered as its own controller — see
	// AgentReconciler's doc comment); this breaks failures down by which
	// step actually failed.
	reconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pulse_operator_reconcile_errors_total",
			Help: "Total number of reconcile errors, by reconciler step.",
		},
		[]string{"step"},
	)

	// observedMemoryBytes is the most recently observed real memory usage
	// (max across replicas — see resource_metrics.go's doc comment) for a
	// component, read from the real metrics.k8s.io API. Item 3 of the
	// autonomy roadmap: advisory-only auto-tuning input, never fabricated —
	// simply absent (no sample set) whenever metrics.k8s.io isn't
	// reachable (e.g. no metrics-server installed), exactly like
	// ClusterInfo's ACM/oauth-proxy detection in cluster_detect.go treats
	// an optional cluster capability that may not exist.
	observedMemoryBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pulse_operator_observed_memory_bytes",
			Help: "Most recently observed real memory usage (max across replicas) for a component, read from metrics.k8s.io. Absent when metrics.k8s.io is unavailable — never fabricated or estimated.",
		},
		[]string{"namespace", "name", "component"},
	)

	// requestedMemoryBytes is the effective resources.requests.memory
	// currently applied to a component's container (agentResources/
	// uiResources' resolved value — spec override or built-in default),
	// for side-by-side comparison with observedMemoryBytes on a dashboard.
	// Needs no cluster API call, so — unlike observedMemoryBytes — this is
	// set on every reconcile.
	requestedMemoryBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pulse_operator_requested_memory_bytes",
			Help: "The effective resources.requests.memory currently applied to a component's container.",
		},
		[]string{"namespace", "name", "component"},
	)
)

func init() {
	metrics.Registry.MustRegister(selfHealActionsTotal, componentReady, reconcileErrorsTotal,
		observedMemoryBytes, requestedMemoryBytes)
}
