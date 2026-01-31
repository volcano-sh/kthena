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
	volcanoV1Beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
)

// GangPolicy defines the gang scheduling configuration.
type GangPolicy struct {
	// MinRoleReplicas defines the minimum number of replicas required for each role
	// in gang scheduling, pods in each role are strictly gang required.
	// This map allows users to specify different minimum replica requirements for different roles.
	// If this field is not set, all roles in the ServingGroup are considered gang required by default.
	// For example if you specify a 2P(prefill) 4D(decode) serving group and set the below gangPolicy:
	// ```yaml
	// gangPolicy:
	//   minRoleReplicas:
	//     prefill: 1
	//     decode: 1
	// ```
	// It will result in the following behavior:
	// At least one prefill and one decode must be scheduled before any of the pods in the serving group can run.
	// And pods within a role must be scheduled together.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="minRoleReplicas is immutable"
	MinRoleReplicas map[string]int32 `json:"minRoleReplicas,omitempty"`
}

// NetworkTopologySpec defines the network topology affinity scheduling policy for the roles and group, it works only when the scheduler supports network topology feature.
type NetworkTopology struct {
	// GroupPolicy defines the network topology scheduling requirement of  all the instances within the `ServingGroup`.
	GroupPolicy *volcanoV1Beta1.NetworkTopologySpec `json:"groupPolicy,omitempty"`

	// RolePolicy defines the fine-grained network topology scheduling requirement for instances of a `role`.
	RolePolicy *volcanoV1Beta1.NetworkTopologySpec `json:"rolePolicy,omitempty"`
}

// Role defines the specific pod instance role that performs the inference task.
type Role struct {
	// The name of a role. Name must be unique within an ServingGroup
	// +kubebuilder:validation:MaxLength=12
	// +kubebuilder:validation:Pattern=^[a-zA-Z0-9]([-a-zA-Z0-9]*[a-zA-Z0-9])?$
	Name string `json:"name"`

	// The number of a certain role.
	// For example, in Disaggregated Prefilling, setting the replica count for both the P and D roles to 1 results in 1P1D deployment configuration.
	// This approach can similarly be applied to configure a xPyD deployment scenario.
	// Default to 1.
	// +optional
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// EntryTemplate defines the template for the entry pod of a role.
	// Required: Currently, a role must have only one entry-pod.
	EntryTemplate PodTemplateSpec `json:"entryTemplate"`

	// WorkerReplicas defines the number for the worker pod of a role.
	// Required: Need to set the number of worker-pod replicas.
	// +optional
	// +kubebuilder:default=0
	WorkerReplicas int32 `json:"workerReplicas,omitempty"`

	// WorkerTemplate defines the template for the worker pod of a role.
	// +optional
	WorkerTemplate *PodTemplateSpec `json:"workerTemplate,omitempty"`
}

// PodTemplateSpec describes the data a pod should have when created from a template
type PodTemplateSpec struct {
	// Object's metadata.
	// +optional
	Metadata *Metadata `json:"metadata,omitempty"`
	// Specification of the desired behavior of the pod.
	// +optional
	Spec corev1.PodSpec `json:"spec,omitempty"`
}

// Metadata is a simplified version of ObjectMeta in Kubernetes.
type Metadata struct {
	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ServingGroup is the smallest unit to complete the inference task
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.gangPolicy) || has(self.gangPolicy)", message="gangPolicy is required once set"
type ServingGroup struct {
	// RestartGracePeriodSeconds defines the grace time for the controller to rebuild the ServingGroup when an error occurs
	// Defaults to 0 (ServingGroup will be rebuilt immediately after an error)
	// +optional
	// +kubebuilder:default=0
	RestartGracePeriodSeconds *int64 `json:"restartGracePeriodSeconds,omitempty"`

	// GangPolicy defines the gang scheduler config.
	// +optional
	GangPolicy *GangPolicy `json:"gangPolicy,omitempty"`

	// NetworkTopology defines the network topology affinity scheduling policy for the roles of the `ServingGroup`,
	// it works only when the scheduler supports network topology-aware scheduling.
	// +optional
	NetworkTopology *NetworkTopology `json:"networkTopology,omitempty"`

	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, y.name == x.name))", message="roles name must be unique"
	Roles []Role `json:"roles"`
}
