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

// AutoscalingPolicyBindingSpec defines the desired state of AutoscalingPolicyBinding.
// +kubebuilder:validation:XValidation:rule="has(self.heterogeneousTarget) != has(self.homogeneousTarget)",message="Either heterogeneousTarget or homogeneousTarget must be set, but not both."
type AutoscalingPolicyBindingSpec struct {
	// PolicyRef references the AutoscalingPolicy that defines the scaling rules and metrics.
	PolicyRef corev1.LocalObjectReference `json:"policyRef"`

	// HeterogeneousTarget enables optimization-based scaling across multiple ModelServing deployments with different hardware capabilities.
	// This approach dynamically adjusts replica distribution across heterogeneous resources (e.g., H100/A100 GPUs) based on overall computing requirements.
	// +optional
	HeterogeneousTarget *HeterogeneousTarget `json:"heterogeneousTarget,omitempty"`

	// HomogeneousTarget enables traditional metric-based scaling for a single ModelServing deployment.
	// This approach adjusts replica count based on monitoring metrics and their target values.
	// +optional
	HomogeneousTarget *HomogeneousTarget `json:"homogeneousTarget,omitempty"`
}

// AutoscalingTargetType defines the type of target for autoscaling operations.
type AutoscalingTargetType string

// MetricEndpoint defines the endpoint configuration for scraping metrics from pods.
type MetricEndpoint struct {
	// Uri defines the HTTP path where metrics are exposed (e.g., "/metrics").
	// +optional
	// +kubebuilder:default="/metrics"
	Uri string `json:"uri,omitempty"`
	// Port defines the network port where metrics are exposed by the pods.
	// +optional
	// +kubebuilder:default=8100
	Port int32 `json:"port,omitempty"`
	// LabelSelector defines additional label-based filtering for pods that expose metric endpoints.
	// For example: Ray Leader Pods expose metrics but worker pods don't, so use `ray.io/ray-node-type: 'raylet'`.
	// When targetRef kind is `ModelServing` or `ModelServing/Role`, `modelserving.volcano.sh/entry: 'true'` is added by default.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// HomogeneousTarget defines the configuration for traditional metric-based autoscaling of a single deployment.
type HomogeneousTarget struct {
	// Target defines the object to be monitored and scaled.
	Target Target `json:"target,omitempty"`
	// MinReplicas defines the minimum number of replicas to maintain.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas defines the maximum number of replicas allowed.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`
}

// HeterogeneousTarget defines the configuration for optimization-based autoscaling across multiple deployments.
type HeterogeneousTarget struct {
	// Params defines the configuration parameters for multiple ModelServing groups to be optimized.
	// +kubebuilder:validation:MinItems=1
	Params []HeterogeneousTargetParam `json:"params,omitempty"`
	// CostExpansionRatePercent defines the percentage rate at which the cost expands during optimization calculations.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=200
	// +optional
	CostExpansionRatePercent int32 `json:"costExpansionRatePercent,omitempty"`
}

// Target defines a ModelServing deployment that can be monitored and scaled.
type Target struct {
	// TargetRef references the target object to be monitored and scaled.
	// Default target GVK is ModelServing. Currently supported kinds: ModelServing.
	TargetRef corev1.ObjectReference `json:"targetRef"`
	// SubTarget defines the sub-target object to be monitored and scaled.
	// Currently supported kinds: `Role` when TargetRef kind is ModelServing.
	// +optional
	SubTarget *SubTarget `json:"subTargets,omitempty"`
	// MetricEndpoint defines the configuration for scraping metrics from the target pods.
	// +optional
	MetricEndpoint MetricEndpoint `json:"metricEndpoint,omitempty"`
}

type SubTarget struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

// HeterogeneousTargetParam defines the configuration parameters for a specific deployment type in heterogeneous scaling.
type HeterogeneousTargetParam struct {
	// Target defines the scaling instance configuration for this deployment type.
	Target Target `json:"target,omitempty"`
	// Cost defines the relative cost factor used in optimization calculations.
	// This factor balances performance requirements against deployment costs.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Cost int32 `json:"cost,omitempty"`
	// MinReplicas defines the minimum number of replicas to maintain for this deployment type.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas defines the maximum number of replicas allowed for this deployment type.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient

// AutoscalingPolicyBinding binds AutoscalingPolicy rules to specific ModelServing deployments.
// It enables either traditional metric-based scaling or multi-target optimization across heterogeneous hardware deployments.
type AutoscalingPolicyBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AutoscalingPolicyBindingSpec   `json:"spec,omitempty"`
	Status AutoscalingPolicyBindingStatus `json:"status,omitempty"`
}

// AutoscalingPolicyBindingStatus defines the observed state of AutoscalingPolicyBinding.
type AutoscalingPolicyBindingStatus struct {
	// Placeholder for future status fields
}

// +kubebuilder:object:root=true

// AutoscalingPolicyBindingList contains a list of AutoscalingPolicyBinding objects.
type AutoscalingPolicyBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoscalingPolicyBinding `json:"items"`
}
