package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageSpec selects the container image for the managed Deployment.
type ImageSpec struct {
	// Repository is the image repository, e.g. "ghcr.io/versuscontrol/versus-incident".
	// +kubebuilder:default="ghcr.io/versuscontrol/versus-incident"
	Repository string `json:"repository,omitempty"`
	// Tag is the image tag. Defaults to a recent published tag.
	// +kubebuilder:default="v1.4.3"
	Tag string `json:"tag,omitempty"`
	// PullPolicy is the image pull policy.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// TelegramSpec configures the Telegram alert channel.
type TelegramSpec struct {
	// Enabled turns the Telegram channel on.
	Enabled bool `json:"enabled,omitempty"`
	// SecretName references a Secret holding keys `telegram_bot_token` and
	// `telegram_chat_id`. Required when Enabled is true.
	SecretName string `json:"secretName,omitempty"`
}

// AgentAISpec points the agent's chat model at an OpenAI-compatible endpoint.
type AgentAISpec struct {
	// Enable turns the LLM analyzer on (required for detect mode to call a model).
	Enable bool `json:"enable,omitempty"`
	// Provider selects the LLM backend: "openai" (default) or "gemini". It
	// maps to the right OpenAI-compatible endpoint internally — no base URL.
	// +kubebuilder:validation:Enum=openai;gemini
	// +kubebuilder:default=openai
	Provider string `json:"provider,omitempty"`
	// Model is the model identifier, e.g. "gemini-2.5-flash-lite".
	// +kubebuilder:default="gpt-4o-mini"
	Model string `json:"model,omitempty"`
	// APIKeySecretName references a Secret holding key `agent_ai_api_key`.
	APIKeySecretName string `json:"apiKeySecretName,omitempty"`
}

// AgentSpec configures the AI SRE agent loop.
type AgentSpec struct {
	// Enable turns the agent on.
	Enable bool `json:"enable,omitempty"`
	// Mode is the agent operating mode.
	// +kubebuilder:validation:Enum=training;shadow;detect
	// +kubebuilder:default=training
	Mode string `json:"mode,omitempty"`
	// PollInterval is how often each source is pulled (Go duration).
	// +kubebuilder:default="15s"
	PollInterval string `json:"pollInterval,omitempty"`
	// AI configures the LLM analyzer.
	AI AgentAISpec `json:"ai,omitempty"`
	// Sources is the list of signal sources the agent tails, mirroring
	// agent_sources.yaml. Empty leaves the agent with no sources.
	Sources []SourceSpec `json:"sources,omitempty"`
}

// SourceSpec is one agent signal source (mirrors an agent_sources.yaml entry).
type SourceSpec struct {
	// Name is a unique source name.
	Name string `json:"name"`
	// Type selects the source kind.
	// +kubebuilder:validation:Enum=file;loki;elasticsearch
	Type string `json:"type"`
	// Enable turns this source on.
	Enable bool `json:"enable,omitempty"`
	// File configures a file-tailing source (type=file).
	File *FileSourceSpec `json:"file,omitempty"`
	// Loki configures a Grafana Loki source (type=loki).
	Loki *LokiSourceSpec `json:"loki,omitempty"`
	// Elasticsearch configures an Elasticsearch source (type=elasticsearch).
	Elasticsearch *ElasticsearchSourceSpec `json:"elasticsearch,omitempty"`
}

// FileSourceSpec tails a log file inside the pod.
type FileSourceSpec struct {
	// Path to the log file.
	Path string `json:"path"`
	// FromBeginning replays the whole file from offset 0 instead of tailing.
	FromBeginning bool `json:"fromBeginning,omitempty"`
	// Format is "text" (default) or "json".
	Format string `json:"format,omitempty"`
}

// LokiSourceSpec reads from a Grafana Loki instance.
type LokiSourceSpec struct {
	// Address is the Loki base URL, e.g. http://loki:3100.
	Address string `json:"address"`
	// Query is a LogQL selector, e.g. {app="api"} |= "error".
	Query string `json:"query"`
	// TenantID sets X-Scope-OrgID for multi-tenant Loki.
	TenantID string `json:"tenantID,omitempty"`
}

// ElasticsearchSourceSpec reads from an Elasticsearch cluster.
type ElasticsearchSourceSpec struct {
	// Addresses is the list of node URLs.
	Addresses []string `json:"addresses"`
	// Index is the index name or pattern.
	Index string `json:"index"`
	// Query is a Lucene query string.
	Query string `json:"query,omitempty"`
}

// VersusIncidentSpec is the desired state of a Versus Incident deployment.
type VersusIncidentSpec struct {
	// Image selects the container image.
	Image ImageSpec `json:"image,omitempty"`
	// Replicas is the desired pod count. Keep at 1 when the agent is enabled
	// (the agent worker is single-writer to the catalog/detect log).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// GatewaySecretName references a Secret holding key `gateway_secret`
	// (gates /api/admin/* and /api/agent/*). Required when the agent is enabled.
	GatewaySecretName string `json:"gatewaySecretName,omitempty"`
	// Telegram configures the Telegram channel.
	Telegram TelegramSpec `json:"telegram,omitempty"`
	// Agent configures the AI SRE agent.
	Agent AgentSpec `json:"agent,omitempty"`
}

// VersusIncidentStatus is the observed state of a Versus Incident deployment.
type VersusIncidentStatus struct {
	// ReadyReplicas mirrors the managed Deployment's ready replica count.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// ObservedGeneration is the .metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions represent the latest observations of the resource's state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vi
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.agent.mode`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VersusIncident is the Schema for the versusincidents API.
type VersusIncident struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VersusIncidentSpec   `json:"spec,omitempty"`
	Status VersusIncidentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VersusIncidentList contains a list of VersusIncident.
type VersusIncidentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VersusIncident `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VersusIncident{}, &VersusIncidentList{})
}
