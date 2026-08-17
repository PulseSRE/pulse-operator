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
	Spec   OpenShiftPulseSpec   `json:"spec,omitempty"`
	Status OpenShiftPulseStatus `json:"status,omitempty"`
}

type OpenShiftPulseSpec struct {
	// +optional
	VertexAI *VertexAIConfig `json:"vertexAI,omitempty"`
	// +optional
	AnthropicAPIKey *APIKeyConfig `json:"anthropicApiKey,omitempty"`
	Agent AgentConfig `json:"agent"`
	// +optional
	UI UIConfig `json:"ui,omitempty"`
	// +optional
	Database DatabaseConfig `json:"database,omitempty"`
	// +optional
	Monitoring MonitoringConfig `json:"monitoring,omitempty"`
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
	// +kubebuilder:default=2
	// +optional
	TrustLevel int32 `json:"trustLevel,omitempty"`
	// +optional
	AllowWriteOperations bool `json:"allowWriteOperations,omitempty"`
	// +optional
	AllowSecretAccess bool `json:"allowSecretAccess,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	MCP MCPConfig `json:"mcp,omitempty"`
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
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
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
}

// +kubebuilder:object:root=true
type OpenShiftPulseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items []OpenShiftPulse `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OpenShiftPulse{}, &OpenShiftPulseList{})
}
