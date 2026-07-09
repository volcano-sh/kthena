# API Reference

## Packages
- [workload.serving.volcano.sh/v1alpha1](#workloadservingvolcanoshv1alpha1)


## workload.serving.volcano.sh/v1alpha1


### Resource Types
- [AutoscalingPolicy](#autoscalingpolicy)
- [AutoscalingPolicyList](#autoscalingpolicylist)
- [ModelBooster](#modelbooster)
- [ModelBoosterList](#modelboosterlist)
- [ModelServing](#modelserving)
- [ModelServingList](#modelservinglist)



#### AutoscalingPolicy



AutoscalingPolicy defines the autoscaling policy configuration for model serving workloads.
It specifies scaling rules, metrics, and behavior for automatic replica adjustment.



_Appears in:_
- [AutoscalingPolicyList](#autoscalingpolicylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `AutoscalingPolicy` | | |
| `spec` _[AutoscalingPolicySpec](#autoscalingpolicyspec)_ |  |  |  |
| `status` _[AutoscalingPolicyStatus](#autoscalingpolicystatus)_ |  |  |  |


#### AutoscalingPolicyBehavior



AutoscalingPolicyBehavior defines the scaling behavior configuration for both scale up and scale down operations.



_Appears in:_
- [AutoscalingPolicySpec](#autoscalingpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `scaleUp` _[AutoscalingPolicyScaleUpPolicy](#autoscalingpolicyscaleuppolicy)_ | ScaleUp defines the policy configuration for scaling up (increasing replicas). |  |  |
| `scaleDown` _[AutoscalingPolicyStablePolicy](#autoscalingpolicystablepolicy)_ | ScaleDown defines the policy configuration for scaling down (decreasing replicas). |  |  |


#### AutoscalingPolicyList



AutoscalingPolicyList contains a list of AutoscalingPolicy objects.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `AutoscalingPolicyList` | | |
| `items` _[AutoscalingPolicy](#autoscalingpolicy) array_ |  |  |  |


#### AutoscalingPolicyMetric



AutoscalingPolicyMetric defines a metric and its target value for scaling decisions.



_Appears in:_
- [AutoscalingPolicySpec](#autoscalingpolicyspec)
- [RoleScalingParam](#rolescalingparam)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name defines the metric key used by the scaling algorithm. |  | MinLength: 1 <br /> |
| `targetValue` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#quantity-resource-api)_ | TargetValue defines the target value for the metric that triggers scaling operations. |  |  |


#### AutoscalingPolicyPanicPolicy



AutoscalingPolicyPanicPolicy defines the emergency scaling policy for handling sudden traffic surges.



_Appears in:_
- [AutoscalingPolicyScaleUpPolicy](#autoscalingpolicyscaleuppolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `percent` _integer_ | Percent defines the maximum percentage of current instances to scale up during panic mode. | 1000 | Maximum: 1000 <br />Minimum: 0 <br /> |
| `panicThresholdPercent` _integer_ | PanicThresholdPercent defines the metric threshold percentage that triggers panic mode.<br />When metrics exceed this percentage of target values, panic mode is activated. | 200 | Maximum: 1000 <br />Minimum: 110 <br /> |


#### AutoscalingPolicyScaleUpPolicy



AutoscalingPolicyScaleUpPolicy defines the scaling up policy configuration.



_Appears in:_
- [AutoscalingPolicyBehavior](#autoscalingpolicybehavior)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `stablePolicy` _[AutoscalingPolicyStablePolicy](#autoscalingpolicystablepolicy)_ | StablePolicy defines the stable scaling policy that uses average metric values over time windows.<br />This policy smooths out short-term fluctuations and avoids unnecessary frequent scaling operations. |  |  |
| `panicPolicy` _[AutoscalingPolicyPanicPolicy](#autoscalingpolicypanicpolicy)_ | PanicPolicy defines the emergency scaling policy for handling sudden traffic spikes.<br />This policy activates during rapid load surges to prevent service degradation or timeouts. |  |  |


#### AutoscalingPolicySpec



AutoscalingPolicySpec defines the desired state of AutoscalingPolicy.

At most one of HomogeneousTarget, HeterogeneousTarget, or DisaggregatedTarget
may be set. When the spec is used standalone (as an AutoscalingPolicy custom
resource), exactly one target must be set; this is enforced by the
autoscalingpolicy validating webhook rather than a CEL rule.



_Appears in:_
- [AutoscalingPolicy](#autoscalingpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tolerancePercent` _integer_ | TolerancePercent defines the percentage of deviation tolerated before scaling actions are triggered.<br />current_replicas represents the current number of instances, while target_replicas represents the expected number of instances calculated from monitoring metrics.<br />Scaling operations are performed only when \|current_replicas - target_replicas\| >= current_replicas * TolerancePercent / 100. | 10 | Maximum: 100 <br />Minimum: 0 <br /> |
| `metrics` _[AutoscalingPolicyMetric](#autoscalingpolicymetric) array_ | Metrics defines the list of metrics used to evaluate scaling decisions.<br />This is the default metric list applied to scalable units. For<br />DisaggregatedTarget, role-level metrics override this list for that role. |  |  |
| `behavior` _[AutoscalingPolicyBehavior](#autoscalingpolicybehavior)_ | Behavior defines the scaling behavior configuration for both scale up and scale down operations. |  |  |
| `homogeneousTarget` _[HomogeneousTarget](#homogeneoustarget)_ | HomogeneousTarget enables traditional metric-based scaling for a single<br />ModelServing deployment (whole-deployment granularity). |  |  |
| `heterogeneousTarget` _[HeterogeneousTarget](#heterogeneoustarget)_ | HeterogeneousTarget enables optimization-based scaling across multiple<br />ModelServing deployments with different hardware capabilities. |  |  |
| `disaggregatedTarget` _[DisaggregatedTarget](#disaggregatedtarget)_ | DisaggregatedTarget enables coordinated autoscaling of roles within a<br />single ModelServing that uses disaggregated serving. |  |  |


#### AutoscalingPolicyStablePolicy



AutoscalingPolicyStablePolicy defines the stable scaling policy for both scale up and scale down operations.



_Appears in:_
- [AutoscalingPolicyBehavior](#autoscalingpolicybehavior)
- [AutoscalingPolicyScaleUpPolicy](#autoscalingpolicyscaleuppolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `instances` _integer_ | Instances defines the maximum absolute number of instances to scale per period. | 1 | Minimum: 0 <br /> |
| `percent` _integer_ | Percent defines the maximum percentage of current instances to scale per period. | 100 | Maximum: 1000 <br />Minimum: 0 <br /> |
| `selectPolicy` _[SelectPolicyType](#selectpolicytype)_ | SelectPolicy determines the selection strategy for scaling operations.<br />'Or' means scaling is performed if either the Percent or Instances requirement is met.<br />'And' means scaling is performed only if both Percent and Instances requirements are met. | Or | Enum: [Or And] <br /> |


#### AutoscalingPolicyStatus



AutoscalingPolicyStatus defines the observed state of AutoscalingPolicy.



_Appears in:_
- [AutoscalingPolicy](#autoscalingpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the most recent generation observed by the controller. |  |  |
| `homogeneousStatus` _[TargetScalingStatus](#targetscalingstatus)_ | HomogeneousStatus reports the observed state when HomogeneousTarget is used. |  |  |
| `disaggregatedStatus` _[DisaggregatedScalingStatus](#disaggregatedscalingstatus)_ | DisaggregatedStatus reports the observed state when DisaggregatedTarget is used. |  |  |
| `heterogeneousStatus` _[TargetScalingStatus](#targetscalingstatus) array_ | HeterogeneousStatus reports the per-target observed state when<br />HeterogeneousTarget is used. |  |  |


#### DisaggregatedScalingStatus



DisaggregatedScalingStatus reports the observed state of a DisaggregatedTarget.



_Appears in:_
- [AutoscalingPolicyStatus](#autoscalingpolicystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `roles` _[TargetScalingStatus](#targetscalingstatus) array_ | Roles reports the observed scaling state per role. |  |  |
| `ratioStatus` _[RoleRatioStatus](#roleratiostatus)_ | RatioStatus reports the observed value of the configured ratio constraint. |  |  |
| `ratioAdjusted` _boolean_ | RatioAdjusted is true when the most recent reconcile had to override the<br />metric-derived replica counts to satisfy the ratio constraint. |  |  |


#### DisaggregatedTarget



DisaggregatedTarget defines coordinated autoscaling for disaggregated
serving roles within a single ModelServing deployment.



_Appears in:_
- [AutoscalingPolicySpec](#autoscalingpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetRef` _[ObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectreference-v1-core)_ | TargetRef references the ModelServing deployment that contains<br />all scalable roles. |  |  |
| `roles` _object (keys:string, values:[RoleScalingParam](#rolescalingparam))_ | Roles defines per-role scaling parameters. The map key is roleName<br />from ModelServing.spec.template.roles[].name. A single role is allowed so<br />users can autoscale one role independently without configuring a P/D pair.<br />RatioConstraint, when set, still requires two distinct roles. |  | MaxProperties: 2 <br />MinProperties: 1 <br /> |
| `ratioConstraint` _[RoleRatioConstraint](#roleratioconstraint)_ | RatioConstraint defines the acceptable ratio range of a single role pair.<br />It enforces that replicas[numeratorRole] / replicas[denominatorRole] stays<br />within [minRatio, maxRatio] when denominator replica is non-zero. |  |  |


#### GangPolicy



GangPolicy defines the gang scheduling configuration.



_Appears in:_
- [ServingGroup](#servinggroup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `minRoleReplicas` _object (keys:string, values:integer)_ | MinRoleReplicas defines the minimum number of replicas required for each role<br />in gang scheduling, pods in each role are strictly gang required.<br />This map allows users to specify different minimum replica requirements for different roles.<br />If this field is not set, all roles in the ServingGroup are considered gang required by default.<br />For example if you specify a 2P(prefill) 4D(decode) serving group and set the below gangPolicy:<br />```yaml<br />gangPolicy:<br />  minRoleReplicas:<br />    prefill: 1<br />    decode: 1<br />```<br />It will result in the following behavior:<br />At least one prefill and one decode must be scheduled before any of the pods in the serving group can run.<br />And pods within a role must be scheduled together. |  |  |


#### HeterogeneousTarget



HeterogeneousTarget defines the configuration for optimization-based autoscaling across multiple deployments.

It distributes replicas across several ModelServing groups with different
hardware (and therefore different Cost) to satisfy the overall demand at the
lowest cost. Each group is described by one entry in Params.

Example (split capacity between an H100 group and a cheaper A100 group):

	heterogeneousTarget:
	  costExpansionRatePercent: 200
	  params:
	    - cost: 100
	      minReplicas: 0
	      maxReplicas: 4
	      target:
	        targetRef:
	          kind: ModelServing
	          name: llama-h100
	    - cost: 60
	      minReplicas: 1
	      maxReplicas: 8
	      target:
	        targetRef:
	          kind: ModelServing
	          name: llama-a100



_Appears in:_
- [AutoscalingPolicySpec](#autoscalingpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `params` _[HeterogeneousTargetParam](#heterogeneoustargetparam) array_ | Params defines the configuration parameters for multiple ModelServing groups to be optimized. |  | MinItems: 1 <br /> |
| `costExpansionRatePercent` _integer_ | CostExpansionRatePercent defines the percentage rate at which the cost expands during optimization calculations.<br />For example, 200 allows the optimizer to spend up to 2x the minimal cost to<br />meet performance targets before refusing to scale further. | 200 | Minimum: 0 <br /> |


#### HeterogeneousTargetParam



HeterogeneousTargetParam defines the configuration parameters for a specific deployment type in heterogeneous scaling.

Example (one expensive H100 group within a HeterogeneousTarget):

	cost: 100
	minReplicas: 0
	maxReplicas: 4
	target:
	  targetRef:
	    kind: ModelServing
	    name: llama-h100



_Appears in:_
- [HeterogeneousTarget](#heterogeneoustarget)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[Target](#target)_ | Target defines the scaling instance configuration for this deployment type. |  |  |
| `cost` _integer_ | Cost defines the relative cost factor used in optimization calculations.<br />This factor balances performance requirements against deployment costs.<br />Values are relative across params, e.g. 100 for an H100 group and 60 for a<br />cheaper A100 group makes the optimizer prefer A100 replicas when adequate. |  | Minimum: 0 <br /> |
| `minReplicas` _integer_ | MinReplicas defines the minimum number of replicas to maintain for this deployment type. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `maxReplicas` _integer_ | MaxReplicas defines the maximum number of replicas allowed for this deployment type. |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |


#### HomogeneousTarget



HomogeneousTarget defines the configuration for traditional metric-based autoscaling of a single deployment.

Example (scale podinfo-ms between 1 and 6 replicas based on RPS):

	homogeneousTarget:
	  minReplicas: 1
	  maxReplicas: 6
	  target:
	    targetRef:
	      kind: ModelServing
	      name: podinfo-ms
	    metricSources:
	      podinfo_rps:
	        prometheus:
	          serverURL: http://prometheus.monitoring.svc:9090
	          query: sum(rate(http_requests_total[2m]))



_Appears in:_
- [AutoscalingPolicySpec](#autoscalingpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[Target](#target)_ | Target defines the object to be monitored and scaled. |  |  |
| `minReplicas` _integer_ | MinReplicas defines the minimum number of replicas to maintain (e.g., 1). |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `maxReplicas` _integer_ | MaxReplicas defines the maximum number of replicas allowed (e.g., 6). |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |


#### Metadata



Metadata is a simplified version of ObjectMeta in Kubernetes.



_Appears in:_
- [PodTemplateSpec](#podtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `labels` _object (keys:string, values:string)_ | Map of string keys and values that can be used to organize and categorize<br />(scope and select) objects. May match selectors of replication controllers<br />and services.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels |  |  |
| `annotations` _object (keys:string, values:string)_ | Annotations is an unstructured key value map stored with a resource that may be<br />set by external tools to store and retrieve arbitrary metadata. They are not<br />queryable and should be preserved when modifying objects.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations |  |  |


#### MetricSource



MetricSource is a discriminated union selecting the metric backend.

Exactly one backend config must be provided:
  - Pod        -> set the pod field only.
  - Prometheus -> set the prometheus field only.

Example (scrape the metric directly from each pod's /metrics endpoint):

	metricSources:
	  gpu_cache_usage:
	    pod:
	      name: vllm:gpu_cache_usage_perc
	      uri: /metrics
	      port: 8000

Example (read the metric from an external Prometheus server):

	metricSources:
	  http_rps:
	    prometheus:
	      serverURL: http://prometheus.monitoring.svc:9090
	      query: sum(rate(http_requests_total[2m]))



_Appears in:_
- [RoleScalingParam](#rolescalingparam)
- [Target](#target)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `pod` _[PodMetricSource](#podmetricsource)_ | Pod configures direct pod endpoint scraping. |  |  |
| `prometheus` _[PrometheusMetricSource](#prometheusmetricsource)_ | Prometheus configures an external Prometheus server as the metric source. |  |  |


#### ModelBackend



ModelBackend defines the configuration for a model backend.



_Appears in:_
- [ModelBoosterSpec](#modelboosterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the backend. Can't duplicate with other ModelBackend name in the same ModelBooster CR.<br />Note: update name will cause the old modelInfer deletion and a new modelInfer creation. |  | Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `type` _[ModelBackendType](#modelbackendtype)_ | Type is the type of the backend. |  | Enum: [vLLM vLLMDisaggregated] <br /> |
| `modelURI` _string_ | ModelURI is the source from which the model is fetched by the downloader init container.<br />Supported schemes:<br />  hf://NAMESPACE/REPO         — Hugging Face Hub repository<br />  ms://NAMESPACE/REPO         — ModelScope repository<br />  s3://BUCKET/PATH            — S3-compatible object storage<br />  obs://BUCKET/PATH           — Huawei Object Storage Service (OBS)<br />  pvc:///CLAIM_NAME/PATH      — path inside a PVC already mounted via CacheURI<br />When using pvc://, the downloader reads the given path from the container filesystem.<br />The downloader init container only mounts the volume specified by CacheURI, so the<br />modelURI path must be reachable through that mount.  Both CacheURI and modelURI must<br />reference the same PVC, and the modelURI path must start with the CacheURI mount point.<br />Example: CacheURI: pvc://model-storage, ModelURI: pvc:///model-storage/models/Qwen |  | Pattern: `^(hf://\|s3://\|obs://\|pvc://\|ms://).+` <br /> |
| `cacheURI` _string_ | CacheURI specifies where the downloaded model is stored and how the storage volume is<br />mounted inside every pod (both the downloader init container and the inference engine).<br />Supported schemes:<br />  pvc://CLAIM_NAME    — PersistentVolumeClaim; the PVC is mounted at /CLAIM_NAME<br />  hostpath://PATH     — host-local directory; mounted at /PATH<br />  (empty)             — an ephemeral EmptyDir volume is used (no persistence)<br />The downloader writes model files under a hashed sub-directory of this mount path.<br />The inference engine reads from the same path.  When ModelURI uses pvc://, CacheURI<br />must also use pvc:// and reference the same PVC so the source path is visible. |  | Pattern: `^(hostpath://\|pvc://).+` <br /> |
| `envFrom` _[EnvFromSource](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#envfromsource-v1-core) array_ | List of sources to populate environment variables in the container.<br />The keys defined within a source must be a C_IDENTIFIER. All invalid keys<br />will be reported as an event when the container is starting. When a key exists in multiple<br />sources, the value associated with the last source will take precedence.<br />Values defined by an Env with a duplicate key will take precedence.<br />Cannot be updated. |  |  |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#envvar-v1-core) array_ | List of environment variables to set in the container.<br />Supported names:<br />"ENDPOINT": When you download model from s3, you have to specify it.<br />"RUNTIME_URL": default is http://localhost:8000<br />"RUNTIME_PORT": default is 8100<br />"RUNTIME_METRICS_PATH": default is /metrics<br />"HF_ENDPOINT":The url of hugging face. Default is https://huggingface.co/<br />"KTHENA_SKIP_ENGINE_DEPENDENCY_INSTALL": default is false. When set to true, skip startup-time pip install of engine connector dependencies.<br />Cannot be updated. |  |  |
| `replicas` _integer_ | Replicas is the fixed number of replicas for the backend. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `workers` _[ModelWorker](#modelworker) array_ | Workers is the list of workers associated with this backend. |  | MaxItems: 1000 <br />MinItems: 1 <br /> |
| `schedulerName` _string_ | SchedulerName defines the name of the scheduler used by ModelServing for this backend. |  |  |
| `runtimeClassName` _string_ | RuntimeClassName refers to a RuntimeClass object in the node.k8s.io group,<br />which should be used to run pods generated for this backend. |  |  |


#### ModelBackendType

_Underlying type:_ _string_

ModelBackendType defines the type of model backend.

_Validation:_
- Enum: [vLLM vLLMDisaggregated]

_Appears in:_
- [ModelBackend](#modelbackend)

| Field | Description |
| --- | --- |
| `vLLM` | ModelBackendTypeVLLM represents a vLLM backend.<br /> |
| `vLLMDisaggregated` | ModelBackendTypeVLLMDisaggregated represents a disaggregated vLLM backend.<br /> |
| `SGLang` | ModelBackendTypeSGLang represents an SGLang backend.<br /> |
| `MindIE` | ModelBackendTypeMindIE represents a MindIE backend.<br /> |
| `MindIEDisaggregated` | ModelBackendTypeMindIEDisaggregated represents a disaggregated MindIE backend.<br /> |


#### ModelBooster



ModelBooster is the Schema for the models API.



_Appears in:_
- [ModelBoosterList](#modelboosterlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelBooster` | | |
| `spec` _[ModelBoosterSpec](#modelboosterspec)_ |  |  |  |
| `status` _[ModelStatus](#modelstatus)_ |  |  |  |


#### ModelBoosterList



ModelBoosterList contains a list of ModelBooster.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelBoosterList` | | |
| `items` _[ModelBooster](#modelbooster) array_ |  |  |  |


#### ModelBoosterSpec



ModelBoosterSpec defines the desired state of ModelBooster.



_Appears in:_
- [ModelBooster](#modelbooster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the model. ModelBooster CR name is restricted by kubernetes, for example, can't contain uppercase letters.<br />So we use this field to specify the ModelBooster name. |  | MaxLength: 64 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `owner` _string_ | Owner is the owner of the model. |  |  |
| `backend` _[ModelBackend](#modelbackend)_ | Backend is the model backend associated with this model.<br />ModelBackend is the minimum unit of inference instance. It can be vLLM or vLLMDisaggregated. |  |  |
| `modelMatch` _[ModelMatch](#modelmatch)_ | ModelMatch defines the predicate used to match LLM inference requests to a given<br />TargetModels. Multiple match conditions are ANDed together, i.e. the match will<br />evaluate to true only if all conditions are satisfied. |  |  |


#### ModelServing



ModelServing is the Schema for the LLM Serving API



_Appears in:_
- [ModelServingList](#modelservinglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelServing` | | |
| `spec` _[ModelServingSpec](#modelservingspec)_ |  |  |  |
| `status` _[ModelServingStatus](#modelservingstatus)_ |  |  |  |




#### ModelServingList



ModelServingList contains a list of ModelServing





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelServingList` | | |
| `items` _[ModelServing](#modelserving) array_ |  |  |  |


#### ModelServingSpec



ModelServingSpec defines the specification of the ModelServing resource.



_Appears in:_
- [ModelServing](#modelserving)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | Number of ServingGroups. That is the number of instances that run serving tasks<br />Default to 1. | 1 |  |
| `schedulerName` _string_ | SchedulerName defines the name of the scheduler used by ModelServing | volcano |  |
| `plugins` _[PluginSpec](#pluginspec) array_ | Plugins defines optional plugin chain to customize serving pods. |  |  |
| `template` _[ServingGroup](#servinggroup)_ | Template defines the template for ServingGroup |  |  |
| `rolloutStrategy` _[RolloutStrategy](#rolloutstrategy)_ | RolloutStrategy defines the strategy that will be applied to update replicas |  |  |
| `recoveryPolicy` _[RecoveryPolicy](#recoverypolicy)_ | RecoveryPolicy defines the recovery policy for the failed Pod to be rebuilt | RoleRecreate | Enum: [ServingGroupRecreate RoleRecreate None] <br /> |


#### ModelServingStatus



ModelServingStatus defines the observed state of ModelServing



_Appears in:_
- [ModelServing](#modelserving)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | observedGeneration is the most recent generation observed for ModelServing. It corresponds to the<br />ModelServing's generation, which is updated on mutation by the API Server. |  |  |
| `replicas` _integer_ | Replicas track the total number of ServingGroup that have been created (updated or not, ready or not) |  |  |
| `currentReplicas` _integer_ | CurrentReplicas is the number of ServingGroup created by the ModelServing controller from the ModelServing version |  |  |
| `updatedReplicas` _integer_ | UpdatedReplicas track the number of ServingGroup that have been updated (ready or not). |  |  |
| `availableReplicas` _integer_ | AvailableReplicas track the number of ServingGroup that are in ready state (updated or not). |  |  |
| `currentRevision` _string_ | CurrentRevision, if not empty, indicates the ControllerRevision version used to generate<br />ServingGroups in the sequence [0,currentReplicas). |  |  |
| `updateRevision` _string_ | UpdateRevision, if not empty, indicates the ControllerRevision version used to generate<br />ServingGroups in the sequence [replicas-updatedReplicas,replicas). |  |  |
| `labelSelector` _string_ | LabelSelector is a label query over pods that should match the replica count. |  |  |


#### ModelStatus



ModelStatus defines the observed state of ModelBooster.



_Appears in:_
- [ModelBooster](#modelbooster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration track of generation |  |  |




#### ModelWorker



ModelWorker defines the model worker configuration.



_Appears in:_
- [ModelBackend](#modelbackend)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[ModelWorkerType](#modelworkertype)_ | Type is the type of the model worker. | server | Enum: [server prefill decode controller coordinator] <br /> |
| `image` _string_ | Image is the container image for the worker. |  |  |
| `replicas` _integer_ | Replicas is the number of replicas for the worker. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `pods` _integer_ | Pods is the number of pods for the worker. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ | Resources specifies the resource requirements for the worker. |  |  |
| `affinity` _[Affinity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#affinity-v1-core)_ | Affinity specifies the affinity rules for scheduling the worker pods. |  |  |
| `tolerations` _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#toleration-v1-core) array_ | Tolerations specifies the tolerations for scheduling the worker pods. |  |  |
| `config` _[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#json-v1-apiextensions-k8s-io)_ | Config contains worker-specific configuration in JSON format.<br />You can find vLLM config here https://docs.vllm.ai/en/stable/configuration/engine_args.html |  |  |


#### ModelWorkerType

_Underlying type:_ _string_

ModelWorkerType defines the type of model worker.

_Validation:_
- Enum: [server prefill decode controller coordinator]

_Appears in:_
- [ModelWorker](#modelworker)

| Field | Description |
| --- | --- |
| `server` | ModelWorkerTypeServer represents a server worker.<br /> |
| `prefill` | ModelWorkerTypePrefill represents a prefill worker.<br /> |
| `decode` | ModelWorkerTypeDecode represents a decode worker.<br /> |
| `controller` | ModelWorkerTypeController represents a controller worker.<br /> |
| `coordinator` | ModelWorkerTypeCoordinator represents a coordinator worker.<br /> |


#### NetworkTopology



NetworkTopologySpec defines the network topology affinity scheduling policy for the roles and group, it works only when the scheduler supports network topology feature.



_Appears in:_
- [ServingGroup](#servinggroup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `groupPolicy` _[NetworkTopologySpec](#networktopologyspec)_ | GroupPolicy defines the network topology scheduling requirement of  all the instances within the `ServingGroup`. |  |  |
| `rolePolicy` _[NetworkTopologySpec](#networktopologyspec)_ | RolePolicy defines the fine-grained network topology scheduling requirement for instances of a `role`. |  |  |


#### PluginScope



PluginScope restricts where a plugin is applied.
Roles is a whitelist; empty means all roles.
Target limits to entry/worker/all pods; empty means all pods.



_Appears in:_
- [PluginSpec](#pluginspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `roles` _string array_ | Roles limits the plugin to the specified role names. |  |  |
| `target` _[PluginTarget](#plugintarget)_ | Target limits the plugin to specific pod target (Entry/Worker/All).<br />kubebuilder:default=All<br />kubebuilder:validation:Enum=\{All,Entry,Worker\} |  |  |


#### PluginSpec



PluginSpec declares a plugin instance attached to a ModelServing.



_Appears in:_
- [ModelServingSpec](#modelservingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name uniquely identifies the plugin instance within the ModelServing. |  |  |
| `type` _[PluginType](#plugintype)_ | Type indicates plugin category. For now, only BuiltIn is supported. | BuiltIn | Enum: [BuiltIn] <br /> |
| `config` _[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#json-v1-apiextensions-k8s-io)_ | Config is an opaque JSON blob interpreted by the plugin implementation. |  |  |
| `scope` _[PluginScope](#pluginscope)_ | Scope optionally narrows where this plugin runs.<br />By default, it runs on all pods. |  |  |


#### PluginTarget

_Underlying type:_ _string_

PluginTarget specifies which pod kinds a plugin applies to.
If empty, it defaults to All.



_Appears in:_
- [PluginScope](#pluginscope)

| Field | Description |
| --- | --- |
| `All` |  |
| `Entry` |  |
| `Worker` |  |


#### PluginType

_Underlying type:_ _string_

PluginType represents the implementation category of a plugin.



_Appears in:_
- [PluginSpec](#pluginspec)

| Field | Description |
| --- | --- |
| `BuiltIn` |  |


#### PodMetricSource



PodMetricSource configures pod-endpoint scraping for a metric.

For each matching Pod, metrics are scraped from the constructed access link and extracted from Prometheus’s text output
for the metric family identified by Name.

Example (the pod exposes "vllm:num_requests_waiting" on :8000/metrics):

	pod:
	  name: vllm:num_requests_waiting
	  uri: /metrics
	  port: 8000
	  labelSelector:
	    matchLabels:
	      role: decode

The resulting scrape URL would look like: http://10.1.2.3:8000/metrics



_Appears in:_
- [MetricSource](#metricsource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the Prometheus metric name matched against labels in the pod's scraped output.<br />Defaults to the policy metric key when omitted.<br />For example, set it to "vllm:gpu_cache_usage_perc" to read that exact series. |  |  |
| `uri` _string_ | Uri defines the HTTP path where metrics are exposed (e.g., "/metrics"). | /metrics |  |
| `port` _integer_ | Port defines the network port where metrics are exposed by the pods (e.g., 8000). | 8100 |  |


#### PodTemplateSpec



PodTemplateSpec describes the data a pod should have when created from a template



_Appears in:_
- [Role](#role)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metadata` _[Metadata](#metadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PodSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#podspec-v1-core)_ | Specification of the desired behavior of the pod. |  |  |


#### PrometheusAuth



PrometheusAuth configures authentication when connecting to an external Prometheus server.

NOTE: This struct describes the intended configuration surface. The runtime
does not honor any of these fields yet; they are reserved for a follow-up
implementation. Setting them today has no effect on Prometheus requests.



_Appears in:_
- [PrometheusMetricSource](#prometheusmetricsource)



#### PrometheusMetricSource



PrometheusMetricSource configures an external Prometheus server as a metric backend.

The Query is executed as an instant query and must return a single scalar or a
single-sample vector; the resulting value drives the scaling decision.

Example:

	prometheus:
	  serverURL: http://kube-prometheus-stack-prometheus.monitoring.svc:9090
	  query: sum(rate(http_requests_total[2m]))



_Appears in:_
- [MetricSource](#metricsource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serverURL` _string_ | ServerURL is the base URL of the Prometheus HTTP API server.<br />Example: "http://prometheus.monitoring.svc:9090". |  | Format: uri <br />MinLength: 1 <br /> |
| `query` _string_ | Query is a PromQL instant-query expression. It must evaluate to a single<br />scalar or a one-element vector, e.g. "avg(rate(vllm:request_latency[1m]))".<br />More Query details refer to https://prometheus.io/docs/prometheus/latest/querying/basics |  | MinLength: 1 <br /> |
| `auth` _[PrometheusAuth](#prometheusauth)_ | Auth holds optional authentication configuration for the Prometheus server. |  |  |


#### RecoveryPolicy

_Underlying type:_ _string_





_Appears in:_
- [ModelServingSpec](#modelservingspec)

| Field | Description |
| --- | --- |
| `ServingGroupRecreate` | ServingGroupRecreate will recreate all the pods in the ServingGroup if<br />1. Any individual pod in the group is recreated; 2. Any containers/init-containers<br />in a pod is restarted. This is to ensure all pods/containers in the group will be<br />started in the same time.<br /> |
| `RoleRecreate` | RoleRecreate will recreate all pods in one Role if<br />1. Any individual pod in the group is recreated; 2. Any containers/init-containers<br />in a pod is restarted.<br /> |
| `None` | NoneRestartPolicy will follow the same behavior as the default pod or deployment.<br /> |


#### Role



Role defines the specific pod instance role that performs the inference task.



_Appears in:_
- [ServingGroup](#servinggroup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | The name of a role. Name must be unique within an ServingGroup |  | MaxLength: 12 <br />Pattern: `^[a-zA-Z0-9]([-a-zA-Z0-9]*[a-zA-Z0-9])?$` <br /> |
| `replicas` _integer_ | The number of a certain role.<br />For example, in Disaggregated Prefilling, setting the replica count for both the P and D roles to 1 results in 1P1D deployment configuration.<br />This approach can similarly be applied to configure a xPyD deployment scenario.<br />Default to 1. | 1 |  |
| `entryTemplate` _[PodTemplateSpec](#podtemplatespec)_ | EntryTemplate defines the template for the entry pod of a role.<br />Required: Currently, a role must have only one entry-pod. |  |  |
| `workerReplicas` _integer_ | WorkerReplicas defines the number for the worker pod of a role.<br />Required: Need to set the number of worker-pod replicas. |  |  |
| `workerTemplate` _[PodTemplateSpec](#podtemplatespec)_ | WorkerTemplate defines the template for the worker pod of a role. |  |  |
| `maxUnavailable` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#intorstring-intstr-util)_ | MaxUnavailable is the maximum number of replicas of this Role that can be<br />unavailable during a RoleRollingUpdate. Value can be an absolute number (ex: 2)<br />or a percentage of this Role's replicas (ex: 50%). Percentages are rounded down.<br />This field is only valid when rolloutStrategy.type is RoleRollingUpdate.<br />When unset, all outdated replicas of this Role are recreated at once. |  | XIntOrString: \{\} <br /> |


#### RoleRatioConstraint



RoleRatioConstraint defines the acceptable ratio range between two roles.



_Appears in:_
- [DisaggregatedTarget](#disaggregatedtarget)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `numeratorRole` _string_ | NumeratorRole is the role on the numerator side of the ratio. |  |  |
| `denominatorRole` _string_ | DenominatorRole is the role on the denominator side of the ratio. |  |  |
| `minRatio` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#quantity-resource-api)_ | MinRatio is the minimum allowed value of<br />replicas[numeratorRole] / replicas[denominatorRole]. |  |  |
| `maxRatio` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#quantity-resource-api)_ | MaxRatio is the maximum allowed value of<br />replicas[numeratorRole] / replicas[denominatorRole]. |  |  |


#### RoleRatioStatus



RoleRatioStatus reports the observed value for the ratio constraint.



_Appears in:_
- [DisaggregatedScalingStatus](#disaggregatedscalingstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `numeratorRole` _string_ |  |  |  |
| `denominatorRole` _string_ |  |  |  |
| `currentRatio` _string_ |  |  |  |


#### RoleScalingParam



RoleScalingParam defines the scaling configuration for one role.



_Appears in:_
- [DisaggregatedTarget](#disaggregatedtarget)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `minReplicas` _integer_ | MinReplicas defines the minimum number of replicas for this role. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `maxReplicas` _integer_ | MaxReplicas defines the maximum number of replicas for this role. |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |
| `metrics` _[AutoscalingPolicyMetric](#autoscalingpolicymetric) array_ | Metrics defines the list of metrics used to evaluate scaling decisions<br />for this role, allowing different roles to scale on different signals.<br />When set, these metrics override spec.metrics for this role. When omitted,<br />the role inherits spec.metrics. A fixed role (minReplicas == maxReplicas)<br />may omit metrics; the autoscaler keeps it at that fixed size and does not<br />collect metrics for it. |  | MinItems: 1 <br /> |
| `metricSources` _object (keys:string, values:[MetricSource](#metricsource))_ | MetricSources declares how each metric is fetched for this role.<br />Keys must match role-level metrics when present, otherwise top-level<br />spec.metrics[].name.<br />Missing keys are treated as missing metrics for that reconcile loop. |  |  |


#### RollingUpdateConfiguration



RollingUpdateConfiguration defines the parameters to be used for ServingGroupRollingUpdate.



_Appears in:_
- [RolloutStrategy](#rolloutstrategy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxUnavailable` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#intorstring-intstr-util)_ | The maximum number of replicas that can be unavailable during the update.<br />Value can be an absolute number (ex: 5) or a percentage of total replicas at the start of update (ex: 10%).<br />Absolute number is calculated from percentage by rounding down.<br />This can not be 0.<br />By default, a fixed value of 1 is used. | 1 | XIntOrString: \{\} <br /> |
| `partition` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#intorstring-intstr-util)_ | Partition indicates the ordinal at which the ModelServing should be partitioned<br />for updates. During a rolling update, all ServingGroups from ordinal Replicas-1 to<br />Partition are updated. All ServingGroups from ordinal Partition-1 to 0 remain untouched.<br />Value can be an absolute number (ex: 5) or a percentage of total replicas (ex: 10%).<br />Absolute number is calculated from percentage by rounding up.<br />The default value is 0. |  | XIntOrString: \{\} <br /> |


#### RolloutStrategy



RolloutStrategy defines the strategy that the ModelServing controller
will use to perform replica updates.



_Appears in:_
- [ModelServingSpec](#modelservingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[RolloutStrategyType](#rolloutstrategytype)_ | Type defines the rollout strategy. Supported values are<br />"ServingGroupRollingUpdate" and "RoleRollingUpdate". If not specified,<br />it defaults to "ServingGroupRollingUpdate".<br />For `RoleRollingUpdate`, the `maxUnavailable` field in each Role will be used to determine the maximum number of role instances that can be unavailable during the update. | ServingGroupRollingUpdate | Enum: [ServingGroupRollingUpdate RoleRollingUpdate] <br /> |
| `rollingUpdateConfiguration` _[RollingUpdateConfiguration](#rollingupdateconfiguration)_ | RollingUpdateConfiguration defines the parameters to be used when type is ServingGroupRollingUpdate.<br />optional |  |  |


#### RolloutStrategyType

_Underlying type:_ _string_

RolloutStrategyType defines the strategy to use to update replicas.
Note that if `recoveryPolicy` is set to `ServingGroupRecreate` and `rolloutStrategyType` is set to `RoleRollingUpdate`,
the entire servingGroup will be deleted during a rolling update because the outdated role is removed.



_Appears in:_
- [RolloutStrategy](#rolloutstrategy)

| Field | Description |
| --- | --- |
| `ServingGroupRollingUpdate` | `ServingGroupRollingUpdate` indicates that ServingGroup replicas will be updated one by one.<br /> |
| `RoleRollingUpdate` | `RoleRollingUpdate` indicates that Role replicas will be updated one by one.<br /> |


#### SelectPolicyType

_Underlying type:_ _string_

SelectPolicyType defines the selection strategy type for scaling operations.

_Validation:_
- Enum: [Or And]

_Appears in:_
- [AutoscalingPolicyStablePolicy](#autoscalingpolicystablepolicy)

| Field | Description |
| --- | --- |
| `Or` |  |
| `And` |  |


#### ServingGroup



ServingGroup is the smallest unit to complete the inference task



_Appears in:_
- [ModelServingSpec](#modelservingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `restartGracePeriodSeconds` _integer_ | RestartGracePeriodSeconds defines the grace time for the controller to rebuild the ServingGroup when an error occurs<br />Defaults to 0 (ServingGroup will be rebuilt immediately after an error) | 0 |  |
| `gangPolicy` _[GangPolicy](#gangpolicy)_ | GangPolicy defines the gang scheduler config. |  |  |
| `networkTopology` _[NetworkTopology](#networktopology)_ | NetworkTopology defines the network topology affinity scheduling policy for the roles of the `ServingGroup`,<br />it works only when the scheduler supports network topology-aware scheduling. |  |  |
| `roles` _[Role](#role) array_ |  |  | MaxItems: 4 <br />MinItems: 1 <br /> |


#### Target



Target defines a ModelServing deployment that can be monitored and scaled.

Example:

	target:
	  targetRef:
	    kind: ModelServing
	    name: podinfo-ms
	  metricSources:
	    podinfo_rps:
	      prometheus:
	        serverURL: http://prometheus.monitoring.svc:9090
	        query: sum(rate(http_requests_total[2m]))



_Appears in:_
- [HeterogeneousTargetParam](#heterogeneoustargetparam)
- [HomogeneousTarget](#homogeneoustarget)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetRef` _[ObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectreference-v1-core)_ | TargetRef references the target object to be monitored and scaled.<br />Default target GVK is ModelServing. Currently supported kinds: ModelServing.<br />Example: kind=ModelServing, name=podinfo-ms. |  |  |
| `metricSources` _object (keys:string, values:[MetricSource](#metricsource))_ | MetricSources declares how to fetch specific metrics for this target.<br />Keys must match AutoscalingPolicy.spec.metrics[].name.<br />Missing keys are treated as missing metrics for that reconcile loop.<br />For example, a key "podinfo_rps" here must correspond to a metric named<br />"podinfo_rps" in the referenced AutoscalingPolicy. |  |  |


#### TargetScalingStatus



TargetScalingStatus reports the observed scaling state of a single scalable
unit (a whole ModelServing, or one role within it).



_Appears in:_
- [AutoscalingPolicyStatus](#autoscalingpolicystatus)
- [DisaggregatedScalingStatus](#disaggregatedscalingstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name identifies the unit when this status appears in a list.<br />It is required for HeterogeneousStatus entries and DisaggregatedStatus roles,<br />and may be empty for HomogeneousStatus because the target is implied. |  |  |
| `currentReplicas` _integer_ | CurrentReplicas is the number of replicas currently observed. |  |  |
| `desiredReplicas` _integer_ | DesiredReplicas is the number of replicas the controller computed from<br />metrics, before ratio enforcement. |  |  |
| `mode` _string_ | Mode reports whether the unit is currently in "Stable" or "Panic" mode. |  |  |


