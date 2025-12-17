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
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// Environment injected to the worker pods.
	EntryAddressEnv = "ENTRY_ADDRESS"
	// WorkerIndexEnv is the environment variable for the worker index.
	// The entry pod always has a worker index of 0, while the other worker pods has a unique index from 1 to GroupSize-1.
	WorkerIndexEnv = "WORKER_INDEX"
	// GroupSizeEnv is the environment variable for the group size.
	GroupSizeEnv = "GROUP_SIZE"
)

// ModelServingSpec defines the specification of the ModelServing resource.
type ModelServingSpec struct {
	// Number of ServingGroups. That is the number of instances that run serving tasks
	// Default to 1.
	//
	// +optional
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// SchedulerName defines the name of the scheduler used by ModelServing
	//
	// +optional
	// +kubebuilder:default=volcano
	SchedulerName string `json:"schedulerName"`

	// Template defines the template for ServingGroup
	Template ServingGroup `json:"template"`

	// RolloutStrategy defines the strategy that will be applied to update replicas
	// +optional
	RolloutStrategy *RolloutStrategy `json:"rolloutStrategy,omitempty"`

	// RecoveryPolicy defines the recovery policy for the failed Pod to be rebuilt
	// +kubebuilder:default=RoleRecreate
	// +kubebuilder:validation:Enum={ServingGroupRecreate,RoleRecreate,None}
	// +optional
	RecoveryPolicy RecoveryPolicy `json:"recoveryPolicy,omitempty"`
}

type RecoveryPolicy string

const (
	// ServingGroupRecreate will recreate all the pods in the ServingGroup if
	// 1. Any individual pod in the group is recreated; 2. Any containers/init-containers
	// in a pod is restarted. This is to ensure all pods/containers in the group will be
	// started in the same time.
	ServingGroupRecreate RecoveryPolicy = "ServingGroupRecreate"

	// RoleRecreate will recreate all pods in one Role if
	// 1. Any individual pod in the group is recreated; 2. Any containers/init-containers
	// in a pod is restarted.
	RoleRecreate RecoveryPolicy = "RoleRecreate"

	// NoneRestartPolicy will follow the same behavior as the default pod or deployment.
	NoneRestartPolicy RecoveryPolicy = "None"
)

// RolloutStrategy defines the strategy that the ModelServing controller
// will use to perform replica updates.
type RolloutStrategy struct {
	// Type defines the rollout strategy, it can only be “ServingGroupRollingUpdate” for now.
	//
	// +kubebuilder:validation:Enum={ServingGroupRollingUpdate}
	// +kubebuilder:default=ServingGroupRollingUpdate
	Type RolloutStrategyType `json:"type"`

	// RollingUpdateConfiguration defines the parameters to be used when type is RollingUpdateStrategyType.
	// optional
	RollingUpdateConfiguration *RollingUpdateConfiguration `json:"rollingUpdateConfiguration,omitempty"`
}

type RolloutStrategyType string

const (
	// ServingGroupRollingUpdate indicates that ServingGroup replicas will be updated one by one.
	ServingGroupRollingUpdate RolloutStrategyType = "ServingGroupRollingUpdate"
)

// RollingUpdateConfiguration defines the parameters to be used for RollingUpdateStrategyType.
type RollingUpdateConfiguration struct {
	// The maximum number of replicas that can be unavailable during the update.
	// Value can be an absolute number (ex: 5) or a percentage of total replicas at the start of update (ex: 10%).
	// Absolute number is calculated from percentage by rounding down.
	// This can not be 0 if MaxSurge is 0.
	// By default, a fixed value of 1 is used.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:default=1
	MaxUnavailable intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// The maximum number of replicas that can be scheduled above the original number of
	// replicas.
	// Value can be an absolute number (ex: 5) or a percentage of total replicas at
	// the start of the update (ex: 10%).
	// Absolute number is calculated from percentage by rounding up.
	// By default, a value of 0 is used.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:default=0
	MaxSurge intstr.IntOrString `json:"maxSurge,omitempty"`
	// Partition indicates the ordinal at which the ModelServing should be partitioned
	// for updates. During a rolling update, all ServingGroups from ordinal Replicas-1 to
	// Partition are updated. All ServingGroups from ordinal Partition-1 to 0 remain untouched.
	// The default value is 0.
	// +optional
	Partition *int32 `json:"partition,omitempty"`
}

type ModelServingConditionType string

// There is a condition type of a modelServing
const (
	// ModelServingSetAvailable means the modelServing is available,
	// at least the minimum available groups are up and running.
	ModelServingAvailable ModelServingConditionType = "Available"

	// The ModelServing enters the ModelServingSetProgressing state whenever there are ongoing changes,
	// such as the creation of new groups or the scaling of pods within a group.
	// A group remains in the progressing state until all its pods become ready.
	// As long as at least one group is progressing, the entire ModelServing Set is also considered progressing.
	ModelServingProgressing ModelServingConditionType = "Progressing"

	// ModelServingSetUpdateInProgress indicates that modelServing is performing a rolling update.
	// When the entry or worker template is updated, modelServing controller enters the upgrade process and
	// UpdateInProgress is set to true.
	ModelServingUpdateInProgress ModelServingConditionType = "UpdateInProgress"
)

// ModelServingStatus defines the observed state of ModelServing
type ModelServingStatus struct {
	// observedGeneration is the most recent generation observed for ModelServing. It corresponds to the
	// ModelServing's generation, which is updated on mutation by the API Server.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas track the total number of ServingGroup that have been created (updated or not, ready or not)
	Replicas int32 `json:"replicas,omitempty"`

	// CurrentReplicas is the number of ServingGroup created by the ModelServing controller from the ModelServing version
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// UpdatedReplicas track the number of ServingGroup that have been updated (ready or not).
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// AvailableReplicas track the number of ServingGroup that are in ready state (updated or not).
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// Conditions track the condition of the ModelServing.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LabelSelector is a label query over pods that should match the replica count.
	LabelSelector string `json:"labelSelector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.labelSelector
// +kubebuilder:storageversion
// +genclient

// ModelServing is the Schema for the LLM Serving API
type ModelServing struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelServingSpec   `json:"spec,omitempty"`
	Status            ModelServingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelServingList contains a list of ModelServing
type ModelServingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelServing `json:"items"`
}
