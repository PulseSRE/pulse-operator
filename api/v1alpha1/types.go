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
	ProjectID        string `json:"projectId"`
	Region           string `json:"region,omitempty"`
	CredentialSecret string `json:"credentialSecret,omitempty"`
}

type APIKeyConfig struct {
	ExistingSecret string `json:"existingSecret"`
}

type AgentConfig struct {
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
