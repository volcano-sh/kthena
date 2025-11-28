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
	// PolicyRef references the autoscaling policy to be optimized scaling base on multiple targets.
	PolicyRef corev1.LocalObjectReference `json:"policyRef"`

	// It dynamically adjusts replicas across different ModelServing objects based on overall computing power requirements - referred to as "optimize" behavior in the code.
	// For example:
	// When dealing with two types of ModelServing objects corresponding to heterogeneous hardware resources with different computing capabilities (e.g., H100/A100), the "optimize" behavior aims to:
	// Dynamically adjust the deployment ratio of H100/A100 instances based on real-time computing power demands
	// Use integer programming and similar methods to precisely meet computing requirements
	// Maximize hardware utilization efficiency
	HeterogeneousTarget *HeterogeneousTarget `json:"heterogeneousTarget,omitempty"`

	// Adjust the number of related instances based on specified monitoring metrics and their target values.
	HomogeneousTarget *HomogeneousTarget `json:"homogeneousTarget,omitempty"`
}

type AutoscalingTargetType string

type MetricEndpoint struct {
	// The metric uri, e.g. /metrics
	// +optional
	// +kubebuilder:default="/metrics"
	Uri string `json:"uri,omitempty"`
	// The port of pods exposing metric endpoints
	// +optional
	// +kubebuilder:default=8100
	Port int32 `json:"port,omitempty"`
	// LabelSelector is the additional selector to filter the pods exposing metric endpoints.
	// For example, Ray Leader Pod exposes the interface for retrieving monitoring metrics, but ray worker pods do not, so the labelSelector will be added `ray.io/ray-node-type: 'raylet'`.
	// When the targetRef kind is `ModelServing` or `ModelServing/Role`, the labelSelector will be added `modelserving.volcano.sh/entry: 'true'` default.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

type HomogeneousTarget struct {
	// Target represents the objects be monitored and scaled.
	Target Target `json:"target,omitempty"`
	// MinReplicas is the minimum number of replicas.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas is the maximum number of replicas.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`
}

type HeterogeneousTarget struct {
	// Parameters of multiple Model Serving Groups to be optimized.
	// +kubebuilder:validation:MinItems=1
	Params []HeterogeneousTargetParam `json:"params,omitempty"`
	// CostExpansionRatePercent is the percentage rate at which the cost expands.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=200
	// +optional
	CostExpansionRatePercent int32 `json:"costExpansionRatePercent,omitempty"`
}

type Target struct {
	// TargetRef references the target object.
	// The default target GVK is ModelServing.
	// Current supported kinds are ModelServing and ModelServing/Role.
	TargetRef corev1.ObjectReference `json:"targetRef"`
	// MetricEndpoint is the metric source.
	// +optional
	MetricEndpoint MetricEndpoint `json:"metricEndpoint,omitempty"`
}

type HeterogeneousTargetParam struct {
	// The scaling instance configuration
	Target Target `json:"target,omitempty"`
	// Cost is the cost associated with running this backend.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Cost int32 `json:"cost,omitempty"`
	// MinReplicas is the minimum number of replicas for the backend.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas is the maximum number of replicas for the backend.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient

// AutoscalingPolicyBinding is the Schema for the autoscalingpolicybindings API.
type AutoscalingPolicyBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AutoscalingPolicyBindingSpec   `json:"spec,omitempty"`
	Status AutoscalingPolicyBindingStatus `json:"status,omitempty"`
}

// AutoscalingPolicyBindingStatus defines the status of a autoscaling policy binding.
type AutoscalingPolicyBindingStatus struct {
}

// +kubebuilder:object:root=true

// AutoscalingPolicyBindingList contains a list of AutoscalingPolicyBinding.
type AutoscalingPolicyBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoscalingPolicyBinding `json:"items"`
}
