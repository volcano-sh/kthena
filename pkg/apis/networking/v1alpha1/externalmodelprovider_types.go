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

// ExternalProviderType defines the protocol adapter used for an external provider.
// +kubebuilder:validation:Enum=OpenAI;Anthropic
type ExternalProviderType string

const (
	// OpenAI selects the OpenAI-compatible API adapter.
	OpenAI ExternalProviderType = "OpenAI"
	// Anthropic selects the Anthropic-compatible Messages API adapter.
	Anthropic ExternalProviderType = "Anthropic"
)

const (
	// ExternalModelProviderConditionReady reports whether the provider can be used by the router.
	ExternalModelProviderConditionReady = "Ready"
	// ExternalModelProviderConditionCredentialsResolved reports whether the referenced credential is available.
	ExternalModelProviderConditionCredentialsResolved = "CredentialsResolved"

	ExternalModelProviderReasonReady                 = "Ready"
	ExternalModelProviderReasonCredentialNotRequired = "CredentialNotRequired"
	ExternalModelProviderReasonCredentialResolved    = "CredentialResolved"
	ExternalModelProviderReasonCredentialNotFound    = "CredentialNotFound"
	ExternalModelProviderReasonCredentialKeyNotFound = "CredentialKeyNotFound"
)

// ExternalModelProviderSpec defines the desired state of ExternalModelProvider.
type ExternalModelProviderSpec struct {
	// ProviderType selects the protocol adapter used for this provider.
	// +optional
	// +kubebuilder:default=OpenAI
	ProviderType ExternalProviderType `json:"providerType,omitempty"`

	// Model is the actual upstream model name. When set, it overwrites the
	// model in the request, matching ModelServer.Spec.Model behavior.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Model *string `json:"model,omitempty"`

	// BaseURL is the provider endpoint root. External providers must use HTTPS.
	// Example: https://api.deepseek.com
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://.+`
	BaseURL string `json:"baseURL"`

	// InsecureSkipVerify disables server certificate-chain and hostname
	// verification for HTTPS. It does not enable plain HTTP.
	// +optional
	// +kubebuilder:default=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// Auth references a credential Secret in the same namespace.
	// +optional
	Auth *ProviderAuth `json:"auth,omitempty"`

	// Non-sensitive static headers added to upstream requests.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

// ProviderAuth defines how a provider credential is loaded.
// +kubebuilder:validation:XValidation:rule="!has(self.secretRef.optional) || !self.secretRef.optional",message="secretRef.optional must be false or unset"
type ProviderAuth struct {
	// SecretRef references a credential Secret in the same namespace.
	// +kubebuilder:validation:Required
	SecretRef corev1.SecretKeySelector `json:"secretRef"`
}

// ExternalModelProviderStatus defines the observed state of ExternalModelProvider.
type ExternalModelProviderStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient
//
// ExternalModelProvider is the Schema for the externalmodelproviders API.
type ExternalModelProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExternalModelProviderSpec   `json:"spec,omitempty"`
	Status ExternalModelProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExternalModelProviderList contains a list of ExternalModelProvider.
type ExternalModelProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalModelProvider `json:"items"`
}
