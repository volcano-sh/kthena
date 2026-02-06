# API Reference

## Packages
- [networking.serving.volcano.sh/v1alpha1](#networkingservingvolcanoshv1alpha1)


## networking.serving.volcano.sh/v1alpha1


### Resource Types
- [ModelRoute](#modelroute)
- [ModelRouteList](#modelroutelist)
- [ModelServer](#modelserver)
- [ModelServerList](#modelserverlist)



#### BodyMatch



BodyMatch defines the predicate used to match request body content



_Appears in:_
- [ModelMatch](#modelmatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `model` _string_ | Model is the name of the model or lora adapter to match.<br />If this field is not specified, any model or lora adapter will be matched. |  |  |


#### GlobalRateLimit



GlobalRateLimit contains configuration for global rate limiting



_Appears in:_
- [RateLimit](#ratelimit)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `redis` _[RedisConfig](#redisconfig)_ | Redis contains configuration for Redis-based global rate limiting. |  |  |


#### InferenceEngine

_Underlying type:_ _string_

InferenceEngine defines the inference framework used by the modelServer to serve LLM requests.

_Validation:_
- Enum: [vLLM SGLang]

_Appears in:_
- [ModelServerSpec](#modelserverspec)

| Field | Description |
| --- | --- |
| `vLLM` | https://github.com/vllm-project/vllm<br /> |
| `SGLang` | https://github.com/sgl-project/sglang<br /> |


#### KVConnectorSpec



KVConnectorSpec defines KV connector configuration for PD disaggregated routing



_Appears in:_
- [ModelServerSpec](#modelserverspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[KVConnectorType](#kvconnectortype)_ | Type specifies the connector type.<br />If you do not know which type to use, please use "http" as default. | http | Enum: [http lmcache nixl mooncake] <br /> |


#### KVConnectorType

_Underlying type:_ _string_





_Appears in:_
- [KVConnectorSpec](#kvconnectorspec)

| Field | Description |
| --- | --- |
| `http` |  |
| `nixl` |  |
| `lmcache` |  |
| `mooncake` |  |


#### ModelMatch



ModelMatch defines the predicate used to match LLM inference requests to a given
TargetModels. Multiple match conditions are ANDed together, i.e. the match will
evaluate to true only if all conditions are satisfied.



_Appears in:_
- [ModelBoosterSpec](#modelboosterspec)
- [Rule](#rule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `headers` _object (keys:string, values:[StringMatch](#stringmatch))_ | Header to match: prefix, exact, regex<br />If unset, any header will be matched. |  |  |
| `uri` _[StringMatch](#stringmatch)_ | URI to match: prefix, exact, regex<br />If this field is not specified, a default prefix match on the "/" path is provided. |  |  |
| `body` _[BodyMatch](#bodymatch)_ | Body contains conditions to match request body content |  |  |


#### ModelRoute



ModelRoute is the Schema for the Modelroutes API.



_Appears in:_
- [ModelRouteList](#modelroutelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `networking.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelRoute` | | |
| `spec` _[ModelRouteSpec](#modelroutespec)_ |  |  |  |
| `status` _[ModelRouteStatus](#modelroutestatus)_ |  |  |  |


#### ModelRouteList



ModelRouteList contains a list of ModelRoute.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `networking.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelRouteList` | | |
| `items` _[ModelRoute](#modelroute) array_ |  |  |  |


#### ModelRouteSpec



ModelRouteSpec defines the desired state of ModelRoute.



_Appears in:_
- [ModelRoute](#modelroute)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `modelName` _string_ | `model` in the LLM request, it could be a base model name, lora adapter name or even<br />a virtual model name. This field is used to match scenarios other than model adapter name and<br />this field could be empty, but it and  `ModelAdapters` can't both be empty. |  |  |
| `loraAdapters` _string array_ | `model` in the LLM request could be lora adapter name,<br />here is a list of Lora Adapter Names to match. |  | MaxItems: 10 <br /> |
| `parentRefs` _ParentReference array_ | ParentRefs references the Gateways that this ModelRoute should be attached to.<br />If empty, the ModelRoute will be attached to all Gateways in the same namespace. |  |  |
| `rules` _[Rule](#rule) array_ | An ordered list of route rules for LLM traffic. The first rule<br />matching an incoming request will be used.<br />If no rule is matched, an HTTP 404 status code MUST be returned. |  | MaxItems: 16 <br /> |
| `rateLimit` _[RateLimit](#ratelimit)_ | Rate limit for the LLM request based on prompt tokens or output tokens.<br />There is no limitation if this field is not set. |  |  |


#### ModelRouteStatus



ModelRouteStatus defines the observed state of ModelRoute.



_Appears in:_
- [ModelRoute](#modelroute)



#### ModelServer



ModelServer is the Schema for the modelservers API.



_Appears in:_
- [ModelServerList](#modelserverlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `networking.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelServer` | | |
| `spec` _[ModelServerSpec](#modelserverspec)_ |  |  |  |
| `status` _[ModelServerStatus](#modelserverstatus)_ |  |  |  |


#### ModelServerList



ModelServerList contains a list of ModelServer.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `networking.serving.volcano.sh/v1alpha1` | | |
| `kind` _string_ | `ModelServerList` | | |
| `items` _[ModelServer](#modelserver) array_ |  |  |  |


#### ModelServerSpec



ModelServerSpec defines the desired state of ModelServer.



_Appears in:_
- [ModelServer](#modelserver)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `model` _string_ | The real model that the modelServers are running.<br />If the `model` in LLM inference request is different from this field, it should be overwritten by this field.<br />Otherwise, the `model` in LLM inference request will not be mutated. |  | MaxLength: 256 <br /> |
| `inferenceEngine` _[InferenceEngine](#inferenceengine)_ | The inference engine used to serve the model. |  | Enum: [vLLM SGLang] <br />Required: \{\} <br /> |
| `workloadSelector` _[WorkloadSelector](#workloadselector)_ | WorkloadSelector is used to match the model serving instances.<br />Currently, they must be pods within the same namespace as modelServer object. |  | Required: \{\} <br /> |
| `workloadPort` _[WorkloadPort](#workloadport)_ | WorkloadPort defines the port and protocol configuration for the model server. |  |  |
| `trafficPolicy` _[TrafficPolicy](#trafficpolicy)_ | Traffic Policy for accessing the model server instance. |  |  |
| `kvConnector` _[KVConnectorSpec](#kvconnectorspec)_ | KVConnector specifies the KV connector configuration for PD disaggregated routing |  |  |


#### ModelServerStatus



ModelServerStatus defines the observed state of ModelServer.



_Appears in:_
- [ModelServer](#modelserver)



#### PDGroup



PDGroup is used to specify the group key of PD instances.
Also, the labels to match the model serving instances for prefill and decode.



_Appears in:_
- [WorkloadSelector](#workloadselector)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `groupKey` _string_ | GroupKey is the key to distinguish different PD groups.<br />Only PD instances with the same group key and value could be paired. |  |  |
| `prefillLabels` _object (keys:string, values:string)_ | The labels to match the model serving instances for prefill. |  |  |
| `decodeLabels` _object (keys:string, values:string)_ | The labels to match the model serving instances for decode. |  |  |


#### RateLimit







_Appears in:_
- [ModelRouteSpec](#modelroutespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `inputTokensPerUnit` _integer_ | InputTokensPerUnit is the maximum number of input tokens allowed per unit of time.<br />If this field is not set, there is no limit on input tokens. |  | Minimum: 1 <br /> |
| `outputTokensPerUnit` _integer_ | OutputTokensPerUnit is the maximum number of output tokens allowed per unit of time.<br />If this field is not set, there is no limit on output tokens. |  | Minimum: 1 <br /> |
| `unit` _[RateLimitUnit](#ratelimitunit)_ | Unit is the time unit for the rate limit. | second | Enum: [second minute hour day month] <br /> |
| `global` _[GlobalRateLimit](#globalratelimit)_ | Global contains configuration for global rate limiting using distributed storage.<br />If this field is set, global rate limiting will be used; otherwise, local rate limiting will be used. |  |  |


#### RateLimitUnit

_Underlying type:_ _string_



_Validation:_
- Enum: [second minute hour day month]

_Appears in:_
- [RateLimit](#ratelimit)

| Field | Description |
| --- | --- |
| `second` |  |
| `minute` |  |
| `hour` |  |
| `day` |  |
| `month` |  |


#### RedisConfig



RedisConfig contains Redis connection configuration



_Appears in:_
- [GlobalRateLimit](#globalratelimit)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `address` _string_ | Address is the Redis server address in the format "host:port". |  | Required: \{\} <br /> |


#### Retry







_Appears in:_
- [TrafficPolicy](#trafficpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `attempts` _integer_ | The maximum number of times an individual inference request to a model server should be retried.<br />If the maximum number of retries has been done without a successgful response, the request will be considered failed. |  |  |


#### Rule







_Appears in:_
- [ModelRouteSpec](#modelroutespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the rule. |  |  |
| `modelMatch` _[ModelMatch](#modelmatch)_ | Match conditions to be satisfied for the rule to be activated.<br />Empty `modelMatch` means matching all requests. |  |  |
| `targetModels` _[TargetModel](#targetmodel) array_ |  |  | MaxItems: 16 <br /> |


#### StringMatch



StringMatch defines the matching conditions for string fields.
Only one of the fields may be set.



_Appears in:_
- [ModelMatch](#modelmatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `exact` _string_ |  |  |  |
| `prefix` _string_ |  |  |  |
| `regex` _string_ |  |  |  |


#### TargetModel



LLM inference traffic target model



_Appears in:_
- [Rule](#rule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `modelServerName` _string_ | ModelServerName is used to specify the correlated modelServer within the same namespace. |  |  |
| `weight` _integer_ | Weight is used to specify the percentage of traffic should be sent to the target model.<br />The value should be in the range of [0, 100]. | 100 | Maximum: 100 <br />Minimum: 0 <br /> |


#### TrafficPolicy







_Appears in:_
- [ModelServerSpec](#modelserverspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `retry` _[Retry](#retry)_ | The retry policy for the inference request. |  |  |


#### WorkloadPort



WorkloadPort defines the port and protocol configuration for the model server.



_Appears in:_
- [ModelServerSpec](#modelserverspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `port` _integer_ | The port of the model server. The number must be between 1 and 65535. |  | Maximum: 65535 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `protocol` _string_ | The protocol of the model server. Supported values are "http" and "https". | http | Enum: [http https] <br /> |


#### WorkloadSelector



WorkloadSelector is used to match the model serving instances.
Currently, they must be pods within the same namespace as modelServer object.



_Appears in:_
- [ModelServerSpec](#modelserverspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `matchLabels` _object (keys:string, values:string)_ | The base labels to match the model serving instances.<br />All serving instances must match these labels. |  |  |
| `pdGroup` _[PDGroup](#pdgroup)_ | PDGroup is used to further match different roles of the model serving instances,<br />mainly used in case like PD disaggregation. |  |  |


