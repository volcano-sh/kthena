/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelServerSpec defines the desired state of ModelServer.
type ModelServerSpec struct {
	// The real model that the modelServers are running.
	// If the `model` in LLM inference request is different from this field, it should be overwritten by this field.
	// Otherwise, the `model` in LLM inference request will not be mutated.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Model *string `json:"model,omitempty"`
	// The inference engine used to serve the model.
	// +kubebuilder:validation:Required
	InferenceEngine InferenceEngine `json:"inferenceEngine"`
	// WorkloadSelector is used to match the model serving instances.
	// Currently, they must be pods within the same namespace as modelServer object.
	//
	// +kubebuilder:validation:Required
	WorkloadSelector *WorkloadSelector `json:"workloadSelector"`

	// WorkloadPort defines the port and protocol configuration for the model server.
	// +kubebuilder:validation:Required
	WorkloadPort WorkloadPort `json:"workloadPort"`

	// Traffic Policy for accessing the model server instance.
	// +optional
	TrafficPolicy *TrafficPolicy `json:"trafficPolicy,omitempty"`

	// KVConnector specifies the KV connector configuration for PD disaggregated routing
	// +optional
	KVConnector *KVConnectorSpec `json:"kvConnector,omitempty"`

	// APIKeySecretRef references the API key the serving instances require, for
	// example a vLLM engine started with --api-key. The router sends it as a bearer
	// token when discovering the models a pod serves. The Secret must live in this
	// ModelServer's namespace and carry the
	// networking.serving.volcano.sh/external-model-provider-credential label.
	// +optional
	// +kubebuilder:validation:XValidation:rule="has(self.name) && self.name != ''",message="apiKeySecretRef.name is required"
	// +kubebuilder:validation:XValidation:rule="!has(self.optional) || !self.optional",message="apiKeySecretRef.optional must be false or unset"
	APIKeySecretRef *corev1.SecretKeySelector `json:"apiKeySecretRef,omitempty"`
}

// InferenceEngine defines the inference framework used by the modelServer to serve LLM requests.
//
// +kubebuilder:validation:Enum=vLLM;SGLang
type InferenceEngine string

const (
	// https://github.com/vllm-project/vllm
	VLLM InferenceEngine = "vLLM"
	// https://github.com/sgl-project/sglang
	SGLang InferenceEngine = "SGLang"
)

// WorkloadSelector is used to match the model serving instances.
// Currently, they must be pods within the same namespace as modelServer object.
type WorkloadSelector struct {
	// The base labels to match the model serving instances.
	// All serving instances must match these labels.
	// +kube:validation:Required
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
	// PDGroup is used to further match different roles of the model serving instances,
	// mainly used in case like PD disaggregation.
	PDGroup *PDGroup `json:"pdGroup,omitempty"`
}

// PDGroup is used to specify the group key of PD instances.
// Also, the labels to match the model serving instances for prefill and decode.
type PDGroup struct {
	// GroupKey is the key to distinguish different PD groups.
	// Only PD instances with the same group key and value could be paired.
	GroupKey string `json:"groupKey"`
	// The labels to match the model serving instances for prefill.
	PrefillLabels map[string]string `json:"prefillLabels"`
	// The labels to match the model serving instances for decode.
	DecodeLabels map[string]string `json:"decodeLabels"`
}

// WorkloadPort defines the port and protocol configuration for the model server.
type WorkloadPort struct {
	// The port of the model server. The number must be between 1 and 65535.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// The protocol of the model server. Supported values are "http" and "https".
	// +optional
	// +kubebuilder:default="http"
	// +kubebuilder:validation:Enum=http;https
	Protocol string `json:"protocol,omitempty"`
}

type KVConnectorType string

const (
	ConnectorTypeHTTP     KVConnectorType = "http"     // Passthrough without mutating prefil/decode requests
	ConnectorTypeNIXL     KVConnectorType = "nixl"     // Indicates `NixlConnector` in vllm
	ConnectorTypeLMCache  KVConnectorType = "lmcache"  // Indicates `LmcacheConnector` in vllm
	ConnectorTypeMoonCake KVConnectorType = "mooncake" // Indicates `MoonCakeConnector` in vllm-ascend
)

// KVConnectorSpec defines KV connector configuration for PD disaggregated routing
type KVConnectorSpec struct {
	// Type specifies the connector type.
	// If you do not know which type to use, please use "http" as default.
	// +kubebuilder:validation:Enum=http;lmcache;nixl;mooncake
	// +kubebuilder:default="http"
	Type KVConnectorType `json:"type,omitempty"`
}

type TrafficPolicy struct {
	// Timeout bounds how long the router waits for the backend to start responding,
	// covering connection setup, sending the request and waiting for the response
	// headers. It does not bound inference response bodies, so a streamed response
	// may run longer. Connector setup exchanges may use the timeout for their
	// complete response. By default, there is no timeout.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// The retry policy for the inference request.
	// +optional
	Retry *Retry `json:"retry,omitempty"`
	// ConnectionPool configures the upstream HTTP connection pool used when
	// forwarding to this ModelServer's pods. When omitted, a shared default
	// pool is used. Each ModelServer that sets this gets its own isolated pool.
	// +optional
	ConnectionPool *ConnectionPool `json:"connectionPool,omitempty"`

	// TODO: add LoadBalancer policy
}

// ConnectionPool configures the HTTP connection pool for a ModelServer.
type ConnectionPool struct {
	// MaxIdleConnections is the total idle connections across all endpoints.
	// Defaults to 100 when omitted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxIdleConnections *int32 `json:"maxIdleConnections,omitempty"`
	// MaxIdleConnectionsPerHost is the idle connections per pod/endpoint.
	// Defaults to 64 when omitted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxIdleConnectionsPerHost *int32 `json:"maxIdleConnectionsPerHost,omitempty"`
	// MaxConnectionsPerHost limits dialing, active and idle connections per host.
	// 0 means unlimited. Defaults to 0 when omitted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxConnectionsPerHost *int32 `json:"maxConnectionsPerHost,omitempty"`
	// IdleTimeout is how long an idle connection stays open before closing.
	// Defaults to 90s when omitted.
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`
}

type Retry struct {
	// The maximum number of times an individual inference request to a model server should be retried.
	// If the maximum number of retries has been done without a successgful response, the request will be considered failed.
	// +optional
	Attempts int32 `json:"attempts"`
	// RetryInterval is the interval between retries.
	// +kubebuilder:default="100ms"
	RetryInterval *metav1.Duration `json:"retryInterval,omitempty"`
}

// ModelServerStatus defines the observed state of ModelServer.
type ModelServerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient
// +kubebuilder:printcolumn:name="Engine",type="string",JSONPath=".spec.inferenceEngine",description="Inference engine used to serve the model"
// +kubebuilder:printcolumn:name="Port",type="integer",JSONPath=".spec.workloadPort.port",description="Model server port"
// +kubebuilder:printcolumn:name="Protocol",type="string",JSONPath=".spec.workloadPort.protocol",description="Model server protocol"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// ModelServer is the Schema for the modelservers API.
type ModelServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelServerSpec   `json:"spec,omitempty"`
	Status ModelServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelServerList contains a list of ModelServer.
type ModelServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelServer `json:"items"`
}
