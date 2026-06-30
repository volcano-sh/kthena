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
	WorkloadPort WorkloadPort `json:"workloadPort,omitempty"`

	// Traffic Policy for accessing the model server instance.
	// +optional
	TrafficPolicy *TrafficPolicy `json:"trafficPolicy,omitempty"`

	// KVConnector specifies the KV connector configuration for PD disaggregated routing
	// +optional
	KVConnector *KVConnectorSpec `json:"kvConnector,omitempty"`
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
	// The request timeout for the inference request.
	// By default, there is no timeout.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// The retry policy for the inference request.
	// +optional
	Retry *Retry `json:"retry,omitempty"`

	// TODO: add LoadBalancer policy
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
	// observedGeneration is the most recent generation observed for this ModelServer.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// readyReplicas is the number of ready pods that are currently matched by the
	// workloadSelector and have been successfully registered in the router's store.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// matchedReplicas is the total number of pods (ready or not) that match the
	// workloadSelector.
	// +optional
	MatchedReplicas int32 `json:"matchedReplicas,omitempty"`

	// Conditions track the lifecycle of this ModelServer.
	// Types:
	//   - "Ready": true when at least one ready pod is matched and the ModelServer has been
	//     registered with the router store. Signals that `kubectl wait --for=condition=ready`
	//     can be used.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient
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
