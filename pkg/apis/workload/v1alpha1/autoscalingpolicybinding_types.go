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
//
// Exactly one of HeterogeneousTarget or HomogeneousTarget must be set:
//   - HomogeneousTarget   -> scale a single ModelServing by metric targets.
//   - HeterogeneousTarget -> optimize replica distribution across several
//     ModelServing groups with different hardware/cost.
//
// Example (homogeneous, metric-based scaling):
//
//	spec:
//	  policyRef:
//	    name: podinfo-prom-policy
//	  homogeneousTarget:
//	    minReplicas: 1
//	    maxReplicas: 6
//	    target:
//	      targetRef:
//	        kind: ModelServing
//	        name: podinfo-ms
//	      metricSources:
//	        podinfo_rps:
//	          type: Prometheus
//	          prometheus:
//	            serverURL: http://prometheus.monitoring.svc:9090
//	            query: sum(rate(http_requests_total[2m]))
//
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

// MetricSourceType selects the backend from which a metric value is fetched.
// +kubebuilder:validation:Enum=Pod;Prometheus
type MetricSourceType string

const (
	PodMetricSourceType        MetricSourceType = "Pod"
	PrometheusMetricSourceType MetricSourceType = "Prometheus"
)

// MetricSource is a discriminated union selecting the metric backend.
//
// Exactly one backend config must be provided and it must match Type:
//   - Type: Pod        -> set the pod field only.
//   - Type: Prometheus -> set the prometheus field only.
//
// Example (scrape the metric directly from each pod's /metrics endpoint):
//
//	metricSources:
//	  gpu_cache_usage:
//	    type: Pod
//	    pod:
//	      name: vllm:gpu_cache_usage_perc
//	      uri: /metrics
//	      port: 8000
//
// Example (read the metric from an external Prometheus server):
//
//	metricSources:
//	  http_rps:
//	    type: Prometheus
//	    prometheus:
//	      serverURL: http://prometheus.monitoring.svc:9090
//	      query: sum(rate(http_requests_total[2m]))
//
// +kubebuilder:validation:XValidation:rule="self.type != 'Prometheus' || has(self.prometheus)",message="prometheus config is required when type is Prometheus"
// +kubebuilder:validation:XValidation:rule="self.type != 'Pod' || has(self.pod)",message="pod config is required when type is Pod"
// +kubebuilder:validation:XValidation:rule="self.type != 'Prometheus' || !has(self.pod)",message="pod config must not be set when type is Prometheus"
// +kubebuilder:validation:XValidation:rule="self.type != 'Pod' || !has(self.prometheus)",message="prometheus config must not be set when type is Pod"
type MetricSource struct {
	// Type selects the metric source backend.
	// +kubebuilder:default="Pod"
	Type MetricSourceType `json:"type"`
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

// PrometheusTLSConfig holds TLS settings for Prometheus HTTPS connections.
//
// NOTE: Not yet implemented; reserved for future use. The runtime currently
// uses default TLS verification regardless of these fields.
type PrometheusTLSConfig struct {
	// InsecureSkipVerify disables TLS certificate verification.
	//
	// Not yet implemented; reserved for future use.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// CASecret references a Secret key containing a PEM-encoded CA bundle.
	//
	// Not yet implemented; reserved for future use.
	// +optional
	CASecret *corev1.SecretKeySelector `json:"caSecret,omitempty"`
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
//	        type: Prometheus
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

// Target defines a ModelServing deployment that can be monitored and scaled.
//
// Example:
//
//	target:
//	  targetRef:
//	    kind: ModelServing
//	    name: podinfo-ms
//	  subTargets:
//	    kind: Role
//	    name: decode
//	  metricSources:
//	    podinfo_rps:
//	      type: Prometheus
//	      prometheus:
//	        serverURL: http://prometheus.monitoring.svc:9090
//	        query: sum(rate(http_requests_total[2m]))
type Target struct {
	// TargetRef references the target object to be monitored and scaled.
	// Default target GVK is ModelServing. Currently supported kinds: ModelServing.
	// Example: kind=ModelServing, name=podinfo-ms.
	TargetRef corev1.ObjectReference `json:"targetRef"`
	// SubTarget defines the sub-target object to be monitored and scaled.
	// Currently supported kinds: `Role` when TargetRef kind is ModelServing.
	// Example: kind=Role, name=decode to scale only the decode role.
	// +optional
	SubTarget *SubTarget `json:"subTargets,omitempty"`
	// MetricSources declares how to fetch specific metrics for this target.
	// Keys must match AutoscalingPolicy.spec.metrics[].name.
	// Missing keys are treated as missing metrics for that reconcile loop.
	// For example, a key "podinfo_rps" here must correspond to a metric named
	// "podinfo_rps" in the referenced AutoscalingPolicy.
	// +optional
	MetricSources map[string]MetricSource `json:"metricSources,omitempty"`
}

// SubTarget identifies a sub-component of a target to scale independently,
// for example a specific Role inside a ModelServing.
//
// Example: kind=Role, name=decode.
type SubTarget struct {
	// Kind is the sub-target kind. Currently supported: "Role".
	Kind string `json:"kind,omitempty"`
	// Name is the sub-target name, e.g. the role name "decode" or "prefill".
	Name string `json:"name,omitempty"`
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
	// Conditions represents the latest available observations of binding state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true

// AutoscalingPolicyBindingList contains a list of AutoscalingPolicyBinding objects.
type AutoscalingPolicyBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutoscalingPolicyBinding `json:"items"`
}
