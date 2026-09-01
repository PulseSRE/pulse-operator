package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OpenShiftPulse is the Schema for the openshiftpulses API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="AgentVersion",type=string,JSONPath=".status.agentVersion"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type OpenShiftPulse struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OpenShiftPulseSpec   `json:"spec,omitempty"`
	Status            OpenShiftPulseStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!(has(self.vertexAI) && has(self.anthropicApiKey))",message="spec.vertexAI and spec.anthropicApiKey are mutually exclusive — set at most one AI backend"
type OpenShiftPulseSpec struct {
	// +optional
	VertexAI *VertexAIConfig `json:"vertexAI,omitempty"`
	// +optional
	AnthropicAPIKey *APIKeyConfig `json:"anthropicApiKey,omitempty"`
	Agent           AgentConfig   `json:"agent"`
	// UI, Database, and Monitoring below all carry a kubebuilder default of an
	// empty object, on top of +optional. CRD structural-schema defaulting only
	// applies a child field's own default (e.g. database.storageSize) when the
	// parent object is itself present in the submitted object. A client that
	// omits the whole ui/database/monitoring key (as any author who never
	// wrote that section would) gets a Go zero-value struct with none of the
	// child defaults applied, silently disabling e.g. the agent's database
	// wiring even though PostgreSQL is provisioned unconditionally. The
	// empty-object default makes the API server synthesize the parent so its
	// children's own defaults can fire.
	// +optional
	// +kubebuilder:default={}
	UI UIConfig `json:"ui,omitempty"`
	// +optional
	// +kubebuilder:default={}
	Database DatabaseConfig `json:"database,omitempty"`
	// +optional
	// +kubebuilder:default={}
	Monitoring MonitoringConfig `json:"monitoring,omitempty"`
	// Temporal provisions a self-hosted Temporal server for durable plan
	// execution and points the agent at it (PULSE_AGENT_TEMPORAL_HOST). It
	// reuses the operator's PostgreSQL: temporal + temporal_visibility
	// databases are created in the same instance by the auto-setup image.
	// +optional
	// +kubebuilder:default={}
	Temporal TemporalConfig `json:"temporal,omitempty"`
}

type TemporalConfig struct {
	// Enabled is *bool for the same omitempty reason MonitoringConfig
	// documents: a plain bool could never be explicitly false.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Image overrides the Temporal auto-setup image.
	// +optional
	Image string `json:"image,omitempty"`
}

type VertexAIConfig struct {
	ProjectID string `json:"projectId"`
	Region    string `json:"region,omitempty"`
	// CredentialSecret is the name of a Secret in the same namespace containing
	// a GCP service account key. The Secret must have a data key named "key.json"
	// holding the JSON key file. When empty, the agent uses Application Default
	// Credentials (ADC), which requires workload identity or a mounted GOOGLE_APPLICATION_CREDENTIALS.
	// Example: kubectl create secret generic gcp-sa-key --from-file=key.json=sa-key.json
	// +optional
	CredentialSecret string `json:"credentialSecret,omitempty"`
}

type APIKeyConfig struct {
	ExistingSecret string `json:"existingSecret"`
}

type AgentConfig struct {
	// Image is the container image for the Pulse Agent.
	// For production deployments, always specify a digest-pinned image reference
	// (e.g. quay.io/amobrem/pulse-agent@sha256:<hash>) rather than a mutable tag.
	// The operator falls back to quay.io/amobrem/pulse-agent:latest when unset,
	// which provides no integrity guarantee.
	// +optional
	Image string `json:"image,omitempty"`
	// TrustLevel controls how much autonomy the agent has: 0=observe, 1=suggest,
	// 2=confirm (default), 3=batch, 4=autonomous. See README.md#trust-levels.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4
	// +optional
	TrustLevel int32 `json:"trustLevel,omitempty"`
	// AdminUsers restricts the endpoints that change how the agent itself
	// behaves — editing skills, resetting the shared inbox, approving a fix
	// that will act on the cluster — to a comma-separated list of usernames,
	// as reported by the OAuth proxy (for the OpenShift kubeadmin account
	// that is "kube:admin", not the display name).
	//
	// Left empty, every authenticated user may do all three. That is the
	// agent's long-standing default, kept so an upgrade cannot lock an
	// existing deployment out of its own skill editor — but it is the wrong
	// posture for a cluster with more than one operator, and the agent logs a
	// warning on every such call to say so.
	// +optional
	AdminUsers string `json:"adminUsers,omitempty"`
	// +optional
	AllowWriteOperations bool `json:"allowWriteOperations,omitempty"`
	// +optional
	AllowSecretAccess bool `json:"allowSecretAccess,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	MCP MCPConfig `json:"mcp,omitempty"`
	// MinOperatorVersion is an optional, admin-set semver constraint: the
	// minimum pulse-operator version required to safely run this agent
	// image. Nothing populates this automatically — it is opt-in and, left
	// unset (the default), never blocks a deployment. See compat.go's doc
	// comment for the real-vs-fabricated-signal investigation behind why
	// this field exists in this shape rather than checking some other
	// signal automatically.
	// +optional
	MinOperatorVersion string `json:"minOperatorVersion,omitempty"`
}

type MCPConfig struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Image overrides the MCP server image. Defaults to the OCP fork image
	// (quay.io/amobrem/pulse-agent:mcp-server, built from github.com/openshift/openshift-mcp-server).
	// +optional
	Image string `json:"image,omitempty"`
	// Toolsets is the comma-separated list of MCP toolsets to enable.
	// Available: cluster-diagnostics, cni-diagnostics, config, core, helm, kcp,
	// kubevirt, netedge, netobserv, oadp, observability/logs, observability/metrics,
	// observability/otelcol, observability/traces, openshift, openshift/mustgather,
	// ossm, ovn-kubernetes, tekton.
	// Defaults to the full SRE set when empty.
	// +optional
	Toolsets string `json:"toolsets,omitempty"`
}

type DatabaseConfig struct {
	// +kubebuilder:default="5Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
	// +optional
	Image string `json:"image,omitempty"`
}

type MonitoringConfig struct {
	// Enabled is a *bool, not bool: a plain `bool` with `omitempty` cannot
	// represent an explicit `false` over the wire (encoding/json's omitempty
	// drops the zero value), which combined with this field's CRD default of
	// `true` made "disable monitoring" unreachable via any typed Go client.
	// nil means "unset" — the CRD default (true) applies once the object is
	// stored; callers reading this field before that round-trip must treat
	// nil the same way (see monitoringEnabled in the controller package).
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

type UIConfig struct {
	// +optional
	Image string `json:"image,omitempty"`
	// +kubebuilder:default=2
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	OAuthProxyImage string `json:"oauthProxyImage,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type OpenShiftPulseStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`
	// +optional
	AgentHealthy bool `json:"agentHealthy,omitempty"`
	// +optional
	DatabaseReady bool `json:"databaseReady,omitempty"`
	// +optional
	UIAvailable bool `json:"uiAvailable,omitempty"`
	// +optional
	RouteHost string `json:"routeHost,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastHealthyAgentImage is the most recent agent container image observed
	// running while the AgentReady condition was True. Used to distinguish an
	// in-progress image upgrade (Phase=Upgrading) from an unplanned failure
	// (Phase=Degraded), and as the rollback target if an upgrade does not
	// become healthy within the operator's bounded upgrade window.
	// +optional
	LastHealthyAgentImage string `json:"lastHealthyAgentImage,omitempty"`
	// LastHealthyUIImage is the UI-container equivalent of LastHealthyAgentImage.
	// +optional
	LastHealthyUIImage string `json:"lastHealthyUIImage,omitempty"`
	// UpgradeStartedAt records when the operator first observed the desired
	// agent or UI image differ from the last known-healthy image. Cleared
	// once the component is healthy again (the new image succeeded, or an
	// auto-rollback completed). Nil when no upgrade is in progress.
	// +optional
	UpgradeStartedAt *metav1.Time `json:"upgradeStartedAt,omitempty"`

	// LastUpgradeDurationSeconds records how long the most recently
	// completed agent/UI image upgrade (Phase=Upgrading, tracked via
	// UpgradeStartedAt above) took to become healthy again. Set once, when
	// the upgrade completes; left at its previous value between upgrades
	// (0 before the first one). The agent Deployment uses the Recreate
	// strategy (see agent_reconciler.go's buildDeploymentSpec and the PR
	// that added this field for why that was deliberately not changed to a
	// rolling strategy) — every agent image change is a real, full
	// stop-then-start outage window that cannot be eliminated, so this
	// field exists to make its size visible and measured instead.
	// +optional
	LastUpgradeDurationSeconds int64 `json:"lastUpgradeDurationSeconds,omitempty"`

	// AgentObservedMemoryBytes and AgentRequestedMemoryBytes are
	// intentionally NOT status fields — see metrics.go's
	// pulse_operator_observed_memory_bytes / pulse_operator_requested_memory_bytes
	// gauges instead. A Prometheus gauge suits this data better than a
	// status field: it is inherently a point-in-time sample (not
	// reconciled/desired state), and CI's rbac-and-crd-sync job re-verifies
	// the CRD/deepcopy on every change to this file, which a
	// frequently-changing numeric field would make noisy for no benefit.
}

// +kubebuilder:object:root=true
type OpenShiftPulseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenShiftPulse `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OpenShiftPulse{}, &OpenShiftPulseList{})
}
