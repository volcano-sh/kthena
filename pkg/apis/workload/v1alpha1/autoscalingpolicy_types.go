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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AutoscalingPolicySpec defines the desired state of AutoscalingPolicy.
//
// At most one of HomogeneousTarget, HeterogeneousTarget, or DisaggregatedTarget
// may be set. When the spec is used standalone (as an AutoscalingPolicy custom
// resource), exactly one target must be set; this is enforced by the
// autoscalingpolicy validating webhook rather than a CEL rule.
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
	// This is the default metric list applied to scalable units. For
	// DisaggregatedTarget, role-level metrics override this list for that role.
	// +optional
	Metrics []AutoscalingPolicyMetric `json:"metrics,omitempty"`
	// Behavior defines the scaling behavior configuration for both scale up and scale down operations.
	// +optional
	Behavior AutoscalingPolicyBehavior `json:"behavior"`

	// HomogeneousTarget enables traditional metric-based scaling for a single
	// ModelServing deployment (whole-deployment granularity).
	// +optional
	HomogeneousTarget *HomogeneousTarget `json:"homogeneousTarget,omitempty"`

	// HeterogeneousTarget enables optimization-based scaling across multiple
	// ModelServing deployments with different hardware capabilities.
	// +optional
	HeterogeneousTarget *HeterogeneousTarget `json:"heterogeneousTarget,omitempty"`

	// DisaggregatedTarget enables coordinated autoscaling of roles within a
	// single ModelServing that uses disaggregated serving.
	// +optional
	DisaggregatedTarget *DisaggregatedTarget `json:"disaggregatedTarget,omitempty"`
}

// AutoscalingPolicyMetric defines a metric and its target value for scaling decisions.
type AutoscalingPolicyMetric struct {
	// Name defines the metric key used by the scaling algorithm.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
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
	// Instances defines the maximum absolute number of instances to scale per period.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Instances *int32 `json:"instances,omitempty"`
	// Percent defines the maximum percentage of current instances to scale per period.
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
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represents the latest available observations of the policy's state.
	// Well-known condition types include:
	//   - "Ready":                   the policy is actively reconciled.
	//   - "TargetFound":             the referenced ModelServing (and roles) exist.
	//   - "RatioConstraintViolated": the desired counts could not satisfy ratioConstraint
	//                                given the per-role min/max bounds.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// HomogeneousStatus reports the observed state when HomogeneousTarget is used.
	// +optional
	HomogeneousStatus *TargetScalingStatus `json:"homogeneousStatus,omitempty"`

	// DisaggregatedStatus reports the observed state when DisaggregatedTarget is used.
	// +optional
	DisaggregatedStatus *DisaggregatedScalingStatus `json:"disaggregatedStatus,omitempty"`

	// HeterogeneousStatus reports the per-target observed state when
	// HeterogeneousTarget is used.
	// +optional
	HeterogeneousStatus []TargetScalingStatus `json:"heterogeneousStatus,omitempty"`
}

// TargetScalingStatus reports the observed scaling state of a single scalable
// unit (a whole ModelServing, or one role within it).
type TargetScalingStatus struct {
	// Name identifies the unit when this status appears in a list.
	// It is required for HeterogeneousStatus entries and DisaggregatedStatus roles,
	// and may be empty for HomogeneousStatus because the target is implied.
	Name string `json:"name,omitempty"`

	// CurrentReplicas is the number of replicas currently observed.
	CurrentReplicas int32 `json:"currentReplicas"`

	// DesiredReplicas is the number of replicas the controller computed from
	// metrics, before ratio enforcement.
	DesiredReplicas int32 `json:"desiredReplicas"`

	// Mode reports whether the unit is currently in "Stable" or "Panic" mode.
	// +optional
	Mode string `json:"mode,omitempty"`

	// LastScaleTime is the last time the unit was scaled by the controller.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`
}

// DisaggregatedScalingStatus reports the observed state of a DisaggregatedTarget.
type DisaggregatedScalingStatus struct {
	// Roles reports the observed scaling state per role.
	Roles []TargetScalingStatus `json:"roles"`

	// RatioStatus reports the observed value of the configured ratio constraint.
	// +optional
	RatioStatus *RoleRatioStatus `json:"ratioStatus,omitempty"`

	// RatioAdjusted is true when the most recent reconcile had to override the
	// metric-derived replica counts to satisfy the ratio constraint.
	// +optional
	RatioAdjusted bool `json:"ratioAdjusted,omitempty"`
}

// RoleRatioStatus reports the observed value for the ratio constraint.
type RoleRatioStatus struct {
	NumeratorRole   string `json:"numeratorRole"`
	DenominatorRole string `json:"denominatorRole"`
	CurrentRatio    string `json:"currentRatio,omitempty"`
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

// MetricSource is a discriminated union selecting the metric backend.
//
// Exactly one backend config must be provided:
//   - Pod        -> set the pod field only.
//   - Prometheus -> set the prometheus field only.
//
// Example (scrape the metric directly from each pod's /metrics endpoint):
//
//	metricSources:
//	  gpu_cache_usage:
//	    pod:
//	      name: vllm:gpu_cache_usage_perc
//	      uri: /metrics
//	      port: 8000
//
// Example (read the metric from an external Prometheus server):
//
//	metricSources:
//	  http_rps:
//	    prometheus:
//	      serverURL: http://prometheus.monitoring.svc:9090
//	      query: sum(rate(http_requests_total[2m]))
//
// +kubebuilder:validation:XValidation:rule="has(self.pod) || has(self.prometheus)",message="one metric source backend config is required"
// +kubebuilder:validation:XValidation:rule="!(has(self.pod) && has(self.prometheus))",message="pod and prometheus configs are mutually exclusive"
type MetricSource struct {
	// Pod configures direct pod endpoint scraping.
	// +optional
	Pod *PodMetricSource `json:"pod,omitempty"`
	// Prometheus configures an external Prometheus server as the metric source.
	// +optional
	Prometheus *PrometheusMetricSource `json:"prometheus,omitempty"`
}

// PodMetricSource configures pod-endpoint scraping for a metric.
//
// For each matching Pod, metrics are scraped from the constructed access link and extracted from Prometheus’s text output
// for the metric family identified by Name.
//
// Example (the pod exposes "vllm:num_requests_waiting" on :8000/metrics):
//
//	pod:
//	  name: vllm:num_requests_waiting
//	  uri: /metrics
//	  port: 8000
//	  labelSelector:
//	    matchLabels:
//	      role: decode
//
// The resulting scrape URL would look like: http://10.1.2.3:8000/metrics
type PodMetricSource struct {
	// Name is the Prometheus metric name matched against labels in the pod's scraped output.
	// Defaults to the policy metric key when omitted.
	// For example, set it to "vllm:gpu_cache_usage_perc" to read that exact series.
	// +optional
	Name string `json:"name,omitempty"`
	// Uri defines the HTTP path where metrics are exposed (e.g., "/metrics").
	// +optional
	// +kubebuilder:default="/metrics"
	Uri string `json:"uri,omitempty"`
	// Port defines the network port where metrics are exposed by the pods (e.g., 8000).
	// +optional
	// +kubebuilder:default=8100
	Port int32 `json:"port,omitempty"`
	// LabelSelector defines additional filtering for pods exposing this metric.
	// Only pods matching both the target and this selector are scraped, e.g.
	// matchLabels with role=decode to scrape only the decode role's pods.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// PrometheusMetricSource configures an external Prometheus server as a metric backend.
//
// The Query is executed as an instant query and must return a single scalar or a
// single-sample vector; the resulting value drives the scaling decision.
//
// Example:
//
//	prometheus:
//	  serverURL: http://kube-prometheus-stack-prometheus.monitoring.svc:9090
//	  query: sum(rate(http_requests_total[2m]))
type PrometheusMetricSource struct {
	// ServerURL is the base URL of the Prometheus HTTP API server.
	// Example: "http://prometheus.monitoring.svc:9090".
	// +kubebuilder:validation:Format=uri
	// +kubebuilder:validation:MinLength=1
	ServerURL string `json:"serverURL"`
	// Query is a PromQL instant-query expression. It must evaluate to a single
	// scalar or a one-element vector, e.g. "avg(rate(vllm:request_latency[1m]))".
	// More Query details refer to https://prometheus.io/docs/prometheus/latest/querying/basics
	// +kubebuilder:validation:MinLength=1
	Query string `json:"query"`
	// Auth holds optional authentication configuration for the Prometheus server.
	// +optional
	Auth *PrometheusAuth `json:"auth,omitempty"`
}

// PrometheusAuth configures authentication when connecting to an external Prometheus server.
//
// NOTE: This struct describes the intended configuration surface. The runtime
// does not honor any of these fields yet; they are reserved for a follow-up
// implementation. Setting them today has no effect on Prometheus requests.
type PrometheusAuth struct {
}

// Target defines a ModelServing deployment that can be monitored and scaled.
//
// Example:
//
//	target:
//	  targetRef:
//	    kind: ModelServing
//	    name: podinfo-ms
//	  metricSources:
//	    podinfo_rps:
//	      prometheus:
//	        serverURL: http://prometheus.monitoring.svc:9090
//	        query: sum(rate(http_requests_total[2m]))
type Target struct {
	// TargetRef references the target object to be monitored and scaled.
	// Default target GVK is ModelServing. Currently supported kinds: ModelServing.
	// Example: kind=ModelServing, name=podinfo-ms.
	TargetRef corev1.ObjectReference `json:"targetRef"`
	// MetricSources declares how to fetch specific metrics for this target.
	// Keys must match AutoscalingPolicy.spec.metrics[].name.
	// Missing keys are treated as missing metrics for that reconcile loop.
	// For example, a key "podinfo_rps" here must correspond to a metric named
	// "podinfo_rps" in the referenced AutoscalingPolicy.
	// +optional
	MetricSources map[string]MetricSource `json:"metricSources,omitempty"`
}

// HomogeneousTarget defines the configuration for traditional metric-based autoscaling of a single deployment.
//
// Example (scale podinfo-ms between 1 and 6 replicas based on RPS):
//
//	homogeneousTarget:
//	  minReplicas: 1
//	  maxReplicas: 6
//	  target:
//	    targetRef:
//	      kind: ModelServing
//	      name: podinfo-ms
//	    metricSources:
//	      podinfo_rps:
//	        prometheus:
//	          serverURL: http://prometheus.monitoring.svc:9090
//	          query: sum(rate(http_requests_total[2m]))
type HomogeneousTarget struct {
	// Target defines the object to be monitored and scaled.
	Target Target `json:"target,omitempty"`
	// MinReplicas defines the minimum number of replicas to maintain (e.g., 1).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas defines the maximum number of replicas allowed (e.g., 6).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`
}

// HeterogeneousTarget defines the configuration for optimization-based autoscaling across multiple deployments.
//
// It distributes replicas across several ModelServing groups with different
// hardware (and therefore different Cost) to satisfy the overall demand at the
// lowest cost. Each group is described by one entry in Params.
//
// Example (split capacity between an H100 group and a cheaper A100 group):
//
//	heterogeneousTarget:
//	  costExpansionRatePercent: 200
//	  params:
//	    - cost: 100
//	      minReplicas: 0
//	      maxReplicas: 4
//	      target:
//	        targetRef:
//	          kind: ModelServing
//	          name: llama-h100
//	    - cost: 60
//	      minReplicas: 1
//	      maxReplicas: 8
//	      target:
//	        targetRef:
//	          kind: ModelServing
//	          name: llama-a100
type HeterogeneousTarget struct {
	// Params defines the configuration parameters for multiple ModelServing groups to be optimized.
	// +kubebuilder:validation:MinItems=1
	Params []HeterogeneousTargetParam `json:"params,omitempty"`
	// CostExpansionRatePercent defines the percentage rate at which the cost expands during optimization calculations.
	// For example, 200 allows the optimizer to spend up to 2x the minimal cost to
	// meet performance targets before refusing to scale further.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=200
	// +optional
	CostExpansionRatePercent int32 `json:"costExpansionRatePercent,omitempty"`
}

// HeterogeneousTargetParam defines the configuration parameters for a specific deployment type in heterogeneous scaling.
//
// Example (one expensive H100 group within a HeterogeneousTarget):
//
//	cost: 100
//	minReplicas: 0
//	maxReplicas: 4
//	target:
//	  targetRef:
//	    kind: ModelServing
//	    name: llama-h100
type HeterogeneousTargetParam struct {
	// Target defines the scaling instance configuration for this deployment type.
	Target Target `json:"target,omitempty"`
	// Cost defines the relative cost factor used in optimization calculations.
	// This factor balances performance requirements against deployment costs.
	// Values are relative across params, e.g. 100 for an H100 group and 60 for a
	// cheaper A100 group makes the optimizer prefer A100 replicas when adequate.
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

// DisaggregatedTarget defines coordinated autoscaling for disaggregated
// serving roles within a single ModelServing deployment.
// +kubebuilder:validation:XValidation:rule="!has(self.ratioConstraint) || size(self.roles) >= 2",message="roles must contain at least two entries when ratioConstraint is configured"
type DisaggregatedTarget struct {
	// TargetRef references the ModelServing deployment that contains
	// all scalable roles.
	TargetRef corev1.ObjectReference `json:"targetRef"`

	// Roles defines per-role scaling parameters. The map key is roleName
	// from ModelServing.spec.template.roles[].name. A single role is allowed so
	// users can autoscale one role independently without configuring a P/D pair.
	// RatioConstraint, when set, still requires two distinct roles.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=2
	Roles map[string]RoleScalingParam `json:"roles"`

	// RatioConstraint defines the acceptable ratio range of a single role pair.
	// It enforces that replicas[numeratorRole] / replicas[denominatorRole] stays
	// within [minRatio, maxRatio] when denominator replica is non-zero.
	// +optional
	RatioConstraint *RoleRatioConstraint `json:"ratioConstraint,omitempty"`
}

// RoleScalingParam defines the scaling configuration for one role.
type RoleScalingParam struct {
	// MinReplicas defines the minimum number of replicas for this role.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`

	// MaxReplicas defines the maximum number of replicas for this role.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`

	// Metrics defines the list of metrics used to evaluate scaling decisions
	// for this role, allowing different roles to scale on different signals.
	//
	// When set, these metrics override spec.metrics for this role. When omitted,
	// the role inherits spec.metrics. A fixed role (minReplicas == maxReplicas)
	// may omit metrics; the autoscaler keeps it at that fixed size and does not
	// collect metrics for it.
	// +optional
	// +kubebuilder:validation:MinItems=1
	Metrics []AutoscalingPolicyMetric `json:"metrics,omitempty"`

	// MetricSources declares how each metric is fetched for this role.
	// Keys must match role-level metrics when present, otherwise top-level
	// spec.metrics[].name.
	// Missing keys are treated as missing metrics for that reconcile loop.
	// +optional
	MetricSources map[string]MetricSource `json:"metricSources,omitempty"`
}

// RoleRatioConstraint defines the acceptable ratio range between two roles.
// +kubebuilder:validation:XValidation:rule="self.numeratorRole != self.denominatorRole",message="numeratorRole and denominatorRole must differ"
type RoleRatioConstraint struct {
	// NumeratorRole is the role on the numerator side of the ratio.
	NumeratorRole string `json:"numeratorRole"`

	// DenominatorRole is the role on the denominator side of the ratio.
	DenominatorRole string `json:"denominatorRole"`

	// MinRatio is the minimum allowed value of
	// replicas[numeratorRole] / replicas[denominatorRole].
	MinRatio resource.Quantity `json:"minRatio"`

	// MaxRatio is the maximum allowed value of
	// replicas[numeratorRole] / replicas[denominatorRole].
	MaxRatio resource.Quantity `json:"maxRatio"`
}
