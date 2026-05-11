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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ModelRouteSpec defines the desired state of ModelRoute.
// +kubebuilder:validation:XValidation:rule="self.modelName != \"\" || size(self.loraAdapters) > 0", message="ModelName and LoraAdapters cannot both be empty"
type ModelRouteSpec struct {
	// `model` in the LLM request, it could be a base model name, lora adapter name or even
	// a virtual model name. This field is used to match scenarios other than model adapter name and
	// this field could be empty, but it and  `ModelAdapters` can't both be empty.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="modelName is immutable"
	ModelName string `json:"modelName,omitempty"`

	// `model` in the LLM request could be lora adapter name,
	// here is a list of Lora Adapter Names to match.
	//
	// +kubebuilder:validation:MaxItems=10
	LoraAdapters []string `json:"loraAdapters,omitempty"`

	// ParentRefs references the Gateways that this ModelRoute should be attached to.
	// If empty, the ModelRoute will be attached to all Gateways in the same namespace.
	// +optional
	ParentRefs []gatewayv1.ParentReference `json:"parentRefs,omitempty"`

	// An ordered list of route rules for LLM traffic. The first rule
	// matching an incoming request will be used.
	// If no rule is matched, an HTTP 404 status code MUST be returned.
	//
	// +kubebuilder:validation:MaxItems=16
	Rules []*Rule `json:"rules"`

	// Rate limit for the LLM request based on prompt tokens or output tokens.
	// There is no limitation if this field is not set.
	// +optional
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
}

type Rule struct {
	// Name is the name of the rule.
	// +optional
	Name string `json:"name,omitempty"`
	// Match conditions to be satisfied for the rule to be activated.
	// Empty `modelMatch` means matching all requests.
	// +optional
	ModelMatch *ModelMatch `json:"modelMatch,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	TargetModels []*TargetModel `json:"targetModels"`
}

// ModelMatch defines the predicate used to match LLM inference requests to a given
// TargetModels. Multiple match conditions are ANDed together, i.e. the match will
// evaluate to true only if all conditions are satisfied.
type ModelMatch struct {
	// Header to match: prefix, exact, regex
	// If unset, any header will be matched.
	// +optional
	Headers map[string]*StringMatch `json:"headers,omitempty"`

	// URI to match: prefix, exact, regex
	// If this field is not specified, a default prefix match on the "/" path is provided.
	// +optional
	Uri *StringMatch `json:"uri,omitempty"`

	// Body contains conditions to match request body content
	// +optional
	Body *BodyMatch `json:"body,omitempty"`
}

// BodyMatch defines the predicate used to match request body content
type BodyMatch struct {
	// Model is the name of the model or lora adapter to match.
	// If this field is not specified, any model or lora adapter will be matched.
	// +optional
	Model *string `json:"model,omitempty"`
}

// StringMatch defines the matching conditions for string fields.
// Only one of the fields may be set.
type StringMatch struct {
	Exact  *string `json:"exact,omitempty"`
	Prefix *string `json:"prefix,omitempty"`
	Regex  *string `json:"regex,omitempty"`
}

// LLM inference traffic target model
type TargetModel struct {
	// ModelServerName is used to specify the correlated modelServer within the same namespace.
	//
	// +kubebuilder:validation:required
	ModelServerName string `json:"modelServerName"`
	// Weight is used to specify the percentage of traffic should be sent to the target model.
	// The value should be in the range of [0, 100].
	//
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Weight *uint32 `json:"weight,omitempty"`
}

type RateLimit struct {
	// InputTokensPerUnit is the maximum number of input tokens allowed per unit of time.
	// If this field is not set, there is no limit on input tokens.
	// +optional
	// +kubebuilder:validation:Minimum=1
	InputTokensPerUnit *uint32 `json:"inputTokensPerUnit,omitempty"`
	// OutputTokensPerUnit is the maximum number of output tokens allowed per unit of time.
	// If this field is not set, there is no limit on output tokens.
	// +optional
	// +kubebuilder:validation:Minimum=1
	OutputTokensPerUnit *uint32 `json:"outputTokensPerUnit,omitempty"`
	// Unit is the time unit for the rate limit.
	// +kubebuilder:default=second
	// +kubebuilder:validation:Enum=second;minute;hour;day;month
	Unit RateLimitUnit `json:"unit"`
	// Global contains configuration for global rate limiting using distributed storage.
	// If this field is set, global rate limiting will be used; otherwise, local rate limiting will be used.
	// +optional
	Global *GlobalRateLimit `json:"global,omitempty"`
}

// GlobalRateLimit contains configuration for global rate limiting
type GlobalRateLimit struct {
	// Redis contains configuration for Redis-based global rate limiting.
	// +optional
	Redis *RedisConfig `json:"redis,omitempty"`
}

// RedisConfig contains Redis connection configuration
type RedisConfig struct {
	// Address is the Redis server address in the format "host:port".
	// +kubebuilder:validation:Required
	Address string `json:"address"`
}

// +kubebuilder:validation:Enum=second;minute;hour;day;month
type RateLimitUnit string

const (
	Second RateLimitUnit = "second"
	Minute RateLimitUnit = "minute"
	Hour   RateLimitUnit = "hour"
	Day    RateLimitUnit = "day"
	Month  RateLimitUnit = "month"
)

// ModelRouteStatus defines the observed state of ModelRoute.
type ModelRouteStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient
//
// ModelRoute is the Schema for the Modelroutes API.
type ModelRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelRouteSpec   `json:"spec"`
	Status ModelRouteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelRouteList contains a list of ModelRoute.
type ModelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelRoute `json:"items"`
}
