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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AutoscalingPolicySpec defines the desired state of AutoscalingPolicy.
type AutoscalingPolicySpec struct {
	// TolerancePercent defines the percentage of deviation tolerated before scaling actions are triggered.
	// current_replicas represents the current number of instances, while target_replicas represents the expected number of instances calculated from monitoring metrics.
	// Scaling operations are performed only when |current_replicas - target_replicas| >= current_replicas * TolerancePercent / 100.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	TolerancePercent int32 `json:"tolerancePercent"`
	// Metrics defines the list of metrics used to evaluate scaling decisions.
	// +kubebuilder:validation:MinItems=1
	Metrics []AutoscalingPolicyMetric `json:"metrics"`
	// Behavior defines the scaling behavior configuration for both scale up and scale down operations.
	// +optional
	Behavior AutoscalingPolicyBehavior `json:"behavior"`
}

// AutoscalingPolicyMetric defines a metric and its target value for scaling decisions.
type AutoscalingPolicyMetric struct {
	// MetricName defines the name of the metric to monitor for scaling decisions.
	MetricName string `json:"metricName"`
	// TargetValue defines the target value for the metric that triggers scaling operations.
	TargetValue resource.Quantity `json:"targetValue"`
}

// AutoscalingPolicyBehavior defines the scaling behavior configuration for both scale up and scale down operations.
type AutoscalingPolicyBehavior struct {
	// ScaleUp defines the policy configuration for scaling up (increasing replicas).
	// +optional
	ScaleUp AutoscalingPolicyScaleUpPolicy `json:"scaleUp"`
	// ScaleDown defines the policy configuration for scaling down (decreasing replicas).
	// +optional
	ScaleDown AutoscalingPolicyStablePolicy `json:"scaleDown"`
}

// AutoscalingPolicyScaleUpPolicy defines the scaling up policy configuration.
type AutoscalingPolicyScaleUpPolicy struct {
	// StablePolicy defines the stable scaling policy that uses average metric values over time windows.
	// This policy smooths out short-term fluctuations and avoids unnecessary frequent scaling operations.
	// +optional
	StablePolicy AutoscalingPolicyStablePolicy `json:"stablePolicy"`
	// PanicPolicy defines the emergency scaling policy for handling sudden traffic spikes.
	// This policy activates during rapid load surges to prevent service degradation or timeouts.
	// +optional
	PanicPolicy AutoscalingPolicyPanicPolicy `json:"panicPolicy"`
}

// AutoscalingPolicyStablePolicy defines the stable scaling policy for both scale up and scale down operations.
type AutoscalingPolicyStablePolicy struct {
	// Instances defines the maximum absolute number of instances to scale in a single operation.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Instances *int32 `json:"instances,omitempty"`
	// Percent defines the maximum percentage of current instances to scale in a single operation.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=100
	Percent *int32 `json:"percent,omitempty"`
	// Period defines the time duration over which scaling metrics are evaluated.
	// +kubebuilder:default="15s"
	Period *metav1.Duration `json:"period,omitempty"`
	// SelectPolicy determines the selection strategy for scaling operations.
	// 'Or' means scaling is performed if either the Percent or Instances requirement is met.
	// 'And' means scaling is performed only if both Percent and Instances requirements are met.
	// +kubebuilder:default="Or"
	// +optional
	SelectPolicy SelectPolicyType `json:"selectPolicy,omitempty"`
	// StabilizationWindow defines the time window to stabilize scaling actions and prevent rapid oscillations.
	// +optional
	StabilizationWindow *metav1.Duration `json:"stabilizationWindow,omitempty"`
}

// SelectPolicyType defines the selection strategy type for scaling operations.
// +kubebuilder:validation:Enum=Or;And
type SelectPolicyType string

const (
	SelectPolicyOr  SelectPolicyType = "Or"
	SelectPolicyAnd SelectPolicyType = "And"
)

// AutoscalingPolicyPanicPolicy defines the emergency scaling policy for handling sudden traffic surges.
type AutoscalingPolicyPanicPolicy struct {
	// Percent defines the maximum percentage of current instances to scale up during panic mode.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=1000
	Percent *int32 `json:"percent,omitempty"`
	// Period defines the evaluation period for panic mode scaling decisions.
	Period metav1.Duration `json:"period"`
	// PanicThresholdPercent defines the metric threshold percentage that triggers panic mode.
	// When metrics exceed this percentage of target values, panic mode is activated.
	// +kubebuilder:validation:Minimum=110
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=200
	PanicThresholdPercent *int32 `json:"panicThresholdPercent,omitempty"`
	// PanicModeHold defines the duration to remain in panic mode before returning to normal scaling.
	// +kubebuilder:default="60s"
	PanicModeHold *metav1.Duration `json:"panicModeHold,omitempty"`
}

// AutoscalingPolicyStatus defines the observed state of AutoscalingPolicy.
type AutoscalingPolicyStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +genclient

// AutoscalingPolicy defines the autoscaling policy configuration for model serving workloads.
// It specifies scaling rules, metrics, and behavior for automatic replica adjustment.
type AutoscalingPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AutoscalingPolicySpec   `json:"spec,omitempty"`
	Status AutoscalingPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AutoscalingPolicyList contains a list of AutoscalingPolicy objects.
type AutoscalingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoscalingPolicy `json:"items"`
}
