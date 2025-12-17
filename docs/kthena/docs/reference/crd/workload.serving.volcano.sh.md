# API Reference

## Packages
- [workload.serving.volcano.sh/v1alpha1](#workloadservingvolcanoshv1alpha1)


## workload.serving.volcano.sh/v1alpha1


### Resource Types
- [AutoscalingPolicy](#autoscalingpolicy)
- [AutoscalingPolicyBinding](#autoscalingpolicybinding)
- [AutoscalingPolicyBindingList](#autoscalingpolicybindinglist)
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


#### AutoscalingPolicyBinding



AutoscalingPolicyBinding binds AutoscalingPolicy rules to specific ModelServing deployments.
It enables either traditional metric-based scaling or multi-target optimization across heterogeneous hardware deployments.



_Appears in:_
- [AutoscalingPolicyBindingList](#autoscalingpolicybindinglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `AutoscalingPolicyBinding` | | |
| `spec` _[AutoscalingPolicyBindingSpec](#autoscalingpolicybindingspec)_ |  |  |  |
| `status` _[AutoscalingPolicyBindingStatus](#autoscalingpolicybindingstatus)_ |  |  |  |


#### AutoscalingPolicyBindingList



AutoscalingPolicyBindingList contains a list of AutoscalingPolicyBinding objects.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `workload.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `AutoscalingPolicyBindingList` | | |
| `items` _[AutoscalingPolicyBinding](#autoscalingpolicybinding) array_ |  |  |  |


#### AutoscalingPolicyBindingSpec



AutoscalingPolicyBindingSpec defines the desired state of AutoscalingPolicyBinding.



_Appears in:_
- [AutoscalingPolicyBinding](#autoscalingpolicybinding)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `policyRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#localobjectreference-v1-core)_ | PolicyRef references the AutoscalingPolicy that defines the scaling rules and metrics. |  |  |
| `heterogeneousTarget` _[HeterogeneousTarget](#heterogeneoustarget)_ | HeterogeneousTarget enables optimization-based scaling across multiple ModelServing deployments with different hardware capabilities.<br />This approach dynamically adjusts replica distribution across heterogeneous resources (e.g., H100/A100 GPUs) based on overall computing requirements. |  |  |
| `homogeneousTarget` _[HomogeneousTarget](#homogeneoustarget)_ | HomogeneousTarget enables traditional metric-based scaling for a single ModelServing deployment.<br />This approach adjusts replica count based on monitoring metrics and their target values. |  |  |


#### AutoscalingPolicyBindingStatus



AutoscalingPolicyBindingStatus defines the observed state of AutoscalingPolicyBinding.



_Appears in:_
- [AutoscalingPolicyBinding](#autoscalingpolicybinding)



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

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metricName` _string_ | MetricName defines the name of the metric to monitor for scaling decisions. |  |  |
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



_Appears in:_
- [AutoscalingPolicy](#autoscalingpolicy)
- [ModelBoosterSpec](#modelboosterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tolerancePercent` _integer_ | TolerancePercent defines the percentage of deviation tolerated before scaling actions are triggered.<br />current_replicas represents the current number of instances, while target_replicas represents the expected number of instances calculated from monitoring metrics.<br />Scaling operations are performed only when \|current_replicas - target_replicas\| >= current_replicas * TolerancePercent / 100. | 10 | Maximum: 100 <br />Minimum: 0 <br /> |
| `metrics` _[AutoscalingPolicyMetric](#autoscalingpolicymetric) array_ | Metrics defines the list of metrics used to evaluate scaling decisions. |  | MinItems: 1 <br /> |
| `behavior` _[AutoscalingPolicyBehavior](#autoscalingpolicybehavior)_ | Behavior defines the scaling behavior configuration for both scale up and scale down operations. |  |  |


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





#### GangPolicy



GangPolicy defines the gang scheduling configuration.



_Appears in:_
- [ServingGroup](#servinggroup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `minRoleReplicas` _object (keys:string, values:integer)_ | MinRoleReplicas defines the minimum number of replicas required for each role<br />in gang scheduling. This map allows users to specify different<br />minimum replica requirements for different roles.<br />Notice: In practice, when determining the minTaskMember for a podGroup, it takes the minimum value between `MinRoleReplicas[role.Name]` and role.Replicas.<br />If you set:<br />gangPolicy:<br />  minRoleReplicas:<br />    Prefill: 2<br />    Decode: 2<br />And set the roles as:<br />roles:<br />  - name: P<br />    replicas: 1<br />    workerReplicas: 2<br />  - name: D<br />    replicas: 3<br />    workerReplicas: 1<br />The resulting podGroup will have minTaskMember:<br />minTaskMember:<br />  P-0: 3 (1 entry pod + 2 worker pods)<br />  D-0: 4 (1 entry pod + 3 worker pods)<br />  D-1: 4 (1 entry pod + 3 worker pods)<br />The replicase of P is min(minRoleReplicas['P'], role.Replicas) = min(2, 1) = 1<br />The replicase of D is min(minRoleReplicas['D'], role.Replicas) = min(2, 3) = 2<br />Key: role name<br />Value: minimum number of replicas required for that role |  |  |


#### HeterogeneousTarget



HeterogeneousTarget defines the configuration for optimization-based autoscaling across multiple deployments.



_Appears in:_
- [AutoscalingPolicyBindingSpec](#autoscalingpolicybindingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `params` _[HeterogeneousTargetParam](#heterogeneoustargetparam) array_ | Params defines the configuration parameters for multiple ModelServing groups to be optimized. |  | MinItems: 1 <br /> |
| `costExpansionRatePercent` _integer_ | CostExpansionRatePercent defines the percentage rate at which the cost expands during optimization calculations. | 200 | Minimum: 0 <br /> |


#### HeterogeneousTargetParam



HeterogeneousTargetParam defines the configuration parameters for a specific deployment type in heterogeneous scaling.



_Appears in:_
- [HeterogeneousTarget](#heterogeneoustarget)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[Target](#target)_ | Target defines the scaling instance configuration for this deployment type. |  |  |
| `cost` _integer_ | Cost defines the relative cost factor used in optimization calculations.<br />This factor balances performance requirements against deployment costs. |  | Minimum: 0 <br /> |
| `minReplicas` _integer_ | MinReplicas defines the minimum number of replicas to maintain for this deployment type. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `maxReplicas` _integer_ | MaxReplicas defines the maximum number of replicas allowed for this deployment type. |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |


#### HomogeneousTarget



HomogeneousTarget defines the configuration for traditional metric-based autoscaling of a single deployment.



_Appears in:_
- [AutoscalingPolicyBindingSpec](#autoscalingpolicybindingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[Target](#target)_ | Target defines the object to be monitored and scaled. |  |  |
| `minReplicas` _integer_ | MinReplicas defines the minimum number of replicas to maintain. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `maxReplicas` _integer_ | MaxReplicas defines the maximum number of replicas allowed. |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |


#### Metadata



Metadata is a simplified version of ObjectMeta in Kubernetes.



_Appears in:_
- [PodTemplateSpec](#podtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `labels` _object (keys:string, values:string)_ | Map of string keys and values that can be used to organize and categorize<br />(scope and select) objects. May match selectors of replication controllers<br />and services.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels |  |  |
| `annotations` _object (keys:string, values:string)_ | Annotations is an unstructured key value map stored with a resource that may be<br />set by external tools to store and retrieve arbitrary metadata. They are not<br />queryable and should be preserved when modifying objects.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations |  |  |


#### MetricEndpoint



MetricEndpoint defines the endpoint configuration for scraping metrics from pods.



_Appears in:_
- [Target](#target)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `uri` _string_ | Uri defines the HTTP path where metrics are exposed (e.g., "/metrics"). | /metrics |  |
| `port` _integer_ | Port defines the network port where metrics are exposed by the pods. | 8100 |  |


#### ModelBackend



ModelBackend defines the configuration for a model backend.



_Appears in:_
- [ModelBoosterSpec](#modelboosterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the backend. Can't duplicate with other ModelBackend name in the same ModelBooster CR.<br />Note: update name will cause the old modelInfer deletion and a new modelInfer creation. |  | Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `type` _[ModelBackendType](#modelbackendtype)_ | Type is the type of the backend. |  | Enum: [vLLM vLLMDisaggregated] <br /> |
| `modelURI` _string_ | ModelURI is the URI where you download the model. Support hf://, s3://, pvc://. |  | Pattern: `^(hf://\|s3://\|pvc://).+` <br /> |
| `cacheURI` _string_ | CacheURI is the URI where the downloaded model stored. Support hostpath://, pvc://. |  | Pattern: `^(hostpath://\|pvc://).+` <br /> |
| `envFrom` _[EnvFromSource](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#envfromsource-v1-core) array_ | List of sources to populate environment variables in the container.<br />The keys defined within a source must be a C_IDENTIFIER. All invalid keys<br />will be reported as an event when the container is starting. When a key exists in multiple<br />sources, the value associated with the last source will take precedence.<br />Values defined by an Env with a duplicate key will take precedence.<br />Cannot be updated. |  |  |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#envvar-v1-core) array_ | List of environment variables to set in the container.<br />Supported names:<br />"ENDPOINT": When you download model from s3, you have to specify it.<br />"RUNTIME_URL": default is http://localhost:8000<br />"RUNTIME_PORT": default is 8100<br />"RUNTIME_METRICS_PATH": default is /metrics<br />"HF_ENDPOINT":The url of hugging face. Default is https://huggingface.co/<br />Cannot be updated. |  |  |
| `minReplicas` _integer_ | MinReplicas is the minimum number of replicas for the backend. |  | Maximum: 1e+06 <br />Minimum: 0 <br /> |
| `maxReplicas` _integer_ | MaxReplicas is the maximum number of replicas for the backend. |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |
| `workers` _[ModelWorker](#modelworker) array_ | Workers is the list of workers associated with this backend. |  | MaxItems: 1000 <br />MinItems: 1 <br /> |
| `schedulerName` _string_ | SchedulerName defines the name of the scheduler used by ModelServing for this backend. |  |  |


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
| `autoscalingPolicy` _[AutoscalingPolicySpec](#autoscalingpolicyspec)_ | AutoscalingPolicy references the autoscaling policy to be used for this model. |  |  |
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


#### PodTemplateSpec



PodTemplateSpec describes the data a pod should have when created from a template



_Appears in:_
- [Role](#role)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metadata` _[Metadata](#metadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PodSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#podspec-v1-core)_ | Specification of the desired behavior of the pod. |  |  |


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


#### RollingUpdateConfiguration



RollingUpdateConfiguration defines the parameters to be used for RollingUpdateStrategyType.



_Appears in:_
- [RolloutStrategy](#rolloutstrategy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxUnavailable` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#intorstring-intstr-util)_ | The maximum number of replicas that can be unavailable during the update.<br />Value can be an absolute number (ex: 5) or a percentage of total replicas at the start of update (ex: 10%).<br />Absolute number is calculated from percentage by rounding down.<br />This can not be 0 if MaxSurge is 0.<br />By default, a fixed value of 1 is used. | 1 | XIntOrString: \{\} <br /> |
| `maxSurge` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#intorstring-intstr-util)_ | The maximum number of replicas that can be scheduled above the original number of<br />replicas.<br />Value can be an absolute number (ex: 5) or a percentage of total replicas at<br />the start of the update (ex: 10%).<br />Absolute number is calculated from percentage by rounding up.<br />By default, a value of 0 is used. | 0 | XIntOrString: \{\} <br /> |
| `partition` _integer_ | Partition indicates the ordinal at which the ModelServing should be partitioned<br />for updates. During a rolling update, all ServingGroups from ordinal Replicas-1 to<br />Partition are updated. All ServingGroups from ordinal Partition-1 to 0 remain untouched.<br />The default value is 0. |  |  |


#### RolloutStrategy



RolloutStrategy defines the strategy that the ModelServing controller
will use to perform replica updates.



_Appears in:_
- [ModelServingSpec](#modelservingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[RolloutStrategyType](#rolloutstrategytype)_ | Type defines the rollout strategy, it can only be “ServingGroupRollingUpdate” for now. | ServingGroupRollingUpdate | Enum: [ServingGroupRollingUpdate] <br /> |
| `rollingUpdateConfiguration` _[RollingUpdateConfiguration](#rollingupdateconfiguration)_ | RollingUpdateConfiguration defines the parameters to be used when type is RollingUpdateStrategyType.<br />optional |  |  |


#### RolloutStrategyType

_Underlying type:_ _string_





_Appears in:_
- [RolloutStrategy](#rolloutstrategy)

| Field | Description |
| --- | --- |
| `ServingGroupRollingUpdate` | ServingGroupRollingUpdate indicates that ServingGroup replicas will be updated one by one.<br /> |


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


#### SubTarget







_Appears in:_
- [Target](#target)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ |  |  |  |
| `name` _string_ |  |  |  |


#### Target



Target defines a ModelServing deployment that can be monitored and scaled.



_Appears in:_
- [HeterogeneousTargetParam](#heterogeneoustargetparam)
- [HomogeneousTarget](#homogeneoustarget)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetRef` _[ObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectreference-v1-core)_ | TargetRef references the target object to be monitored and scaled.<br />Default target GVK is ModelServing. Currently supported kinds: ModelServing. |  |  |
| `subTargets` _[SubTarget](#subtarget)_ | SubTarget defines the sub-target object to be monitored and scaled.<br />Currently supported kinds: `Role` when TargetRef kind is ModelServing. |  |  |
| `metricEndpoint` _[MetricEndpoint](#metricendpoint)_ | MetricEndpoint defines the configuration for scraping metrics from the target pods. |  |  |


