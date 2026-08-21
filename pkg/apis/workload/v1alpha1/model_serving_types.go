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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	SchedulerName string `json:"schedulerName,omitempty"`

	// Plugins defines optional plugin chain to customize serving pods.
	// +optional
	Plugins []PluginSpec `json:"plugins,omitempty"`

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

	// RevisionHistoryLimit is the maximum number of non-live revisions to retain.
	// Revisions still referenced by the ModelServing or its workloads do not count
	// toward this limit.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
}

type RecoveryPolicy string

// PluginType represents the implementation category of a plugin.
type PluginType string

const (
	PluginTypeBuiltIn PluginType = "BuiltIn"
)

// PluginTarget specifies which pod kinds a plugin applies to.
// If empty, it defaults to All.
type PluginTarget string

const (
	PluginTargetAll    PluginTarget = "All"
	PluginTargetEntry  PluginTarget = "Entry"
	PluginTargetWorker PluginTarget = "Worker"
)

// PluginScope restricts where a plugin is applied.
// Roles is a whitelist; empty means all roles.
// Target limits to entry/worker/all pods; empty means all pods.
type PluginScope struct {
	// Roles limits the plugin to the specified role names.
	// +optional
	Roles []string `json:"roles,omitempty"`
	// Target limits the plugin to specific pod target (Entry/Worker/All).
	// kubebuilder:default=All
	// kubebuilder:validation:Enum={All,Entry,Worker}
	Target PluginTarget `json:"target,omitempty"`
}

// PluginSpec declares a plugin instance attached to a ModelServing.
type PluginSpec struct {
	// Name uniquely identifies the plugin instance within the ModelServing.
	Name string `json:"name"`
	// Type indicates plugin category. For now, only BuiltIn is supported.
	// +kubebuilder:default=BuiltIn
	// +kubebuilder:validation:Enum={BuiltIn}
	Type PluginType `json:"type"`
	// Config is an opaque JSON blob interpreted by the plugin implementation.
	// +optional
	Config *apiextensionsv1.JSON `json:"config,omitempty"`
	// Scope optionally narrows where this plugin runs.
	// By default, it runs on all pods.
	// +optional
	Scope *PluginScope `json:"scope,omitempty"`
}

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
	// Type selects the granularity of rolling updates. Supported values are
	// ServingGroupRollingUpdate and RoleRollingUpdate. It defaults to
	// ServingGroupRollingUpdate.
	//
	// ServingGroupRollingUpdate uses rolloutStrategy.rollingUpdateConfiguration;
	// rolling update settings on individual Roles do not take effect.
	// RoleRollingUpdate uses the rolling update configuration on each Role;
	// rolloutStrategy.rollingUpdateConfiguration must not be set.
	// Kthena performs RoleRollingUpdate across all ServingGroups at the same time.
	// Therefore, we recommend using it only in scenarios with a single ServingGroup.
	//
	// +kubebuilder:default=ServingGroupRollingUpdate
	// +kubebuilder:validation:Enum={ServingGroupRollingUpdate,RoleRollingUpdate}
	Type RolloutStrategyType `json:"type"`

	// RollingUpdateConfiguration configures ServingGroupRollingUpdate.
	// It must not be set when type is RoleRollingUpdate; configure maxUnavailable
	// and partition on each Role instead.
	// +optional
	RollingUpdateConfiguration *RollingUpdateConfiguration `json:"rollingUpdateConfiguration,omitempty"`
}

// RolloutStrategyType defines the strategy to use to update replicas.
// Note that if recoveryPolicy is ServingGroupRecreate and the rollout strategy
// is RoleRollingUpdate, deleting an outdated Role causes its entire ServingGroup
// to be recreated.
type RolloutStrategyType string

const (
	// `ServingGroupRollingUpdate` indicates that ServingGroup replicas will be updated one by one.
	ServingGroupRollingUpdate RolloutStrategyType = "ServingGroupRollingUpdate"

	// `RoleRollingUpdate` indicates that Role replicas will be updated one by one.
	RoleRollingUpdate RolloutStrategyType = "RoleRollingUpdate"
)

// RollingUpdateConfiguration defines availability and partition settings for
// the rollout granularity where it is configured.
type RollingUpdateConfiguration struct {
	// MaxUnavailable is the maximum number of resources that may be
	// unavailable during an update. It can be an absolute number (for example,
	// 5) or a percentage (for example, 10%). A percentage is calculated from
	// ModelServing replicas for ServingGroupRollingUpdate and from the
	// corresponding Role's replicas for RoleRollingUpdate, then rounded down.
	// The value must not resolve to 0. Defaults to 1.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:default=1
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// Partition protects the first N existing replicas in ascending ordinal order
	// from updates. The remaining replicas are eligible for rolling update.
	// For a contiguous ordinal set, this is equivalent to protecting [0, Partition).
	// Value can be an absolute number (ex: 5) or a percentage of total replicas (ex: 10%).
	// Absolute number is calculated from percentage by rounding up.
	// The default value is 0.
	// +kubebuilder:validation:XIntOrString
	// +optional
	Partition *intstr.IntOrString `json:"partition,omitempty"`
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

	// CurrentRevision, if not empty, indicates the ControllerRevision version preserved by
	// ServingGroups that have not been updated.
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`

	// UpdateRevision, if not empty, indicates the ControllerRevision version targeted by
	// the current ModelServing spec.
	// +optional
	UpdateRevision string `json:"updateRevision,omitempty"`

	// CollisionCount tracks hash collisions for ControllerRevision names.
	// +optional
	CollisionCount *int32 `json:"collisionCount,omitempty"`

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
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas",description="Total number of serving groups"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.availableReplicas",description="Number of serving groups that are ready"
// +kubebuilder:printcolumn:name="Updated",type="integer",JSONPath=".status.updatedReplicas",description="Number of serving groups updated to the latest revision"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

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
