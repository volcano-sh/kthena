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

// ModelServingRoleReplicaSpec defines the desired state of ModelServingRoleReplica
type ModelServingRoleReplicaSpec struct {
	// ModelServingRef references the parent ModelServing
	ModelServingRef corev1.LocalObjectReference `json:"modelServingRef"`

	// RoleName specifies the target role (e.g., "prefill", "decode")
	RoleName string `json:"roleName"`

	// Replicas maps to this role's replica count
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// ModelServingRoleReplicaStatus defines the observed state of ModelServingRoleReplica
type ModelServingRoleReplicaStatus struct {
	// Replicas is the actual number of replicas for the role
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// LabelSelector is a label query over pods that should match the replica count.
	// +optional
	LabelSelector string `json:"labelSelector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.labelSelector
// +kubebuilder:storageversion
// +genclient

// ModelServingRoleReplica is the Schema for the modelservingrolereplicas API
type ModelServingRoleReplica struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelServingRoleReplicaSpec   `json:"spec,omitempty"`
	Status ModelServingRoleReplicaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelServingRoleReplicaList contains a list of ModelServingRoleReplica
type ModelServingRoleReplicaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelServingRoleReplica `json:"items"`
}
