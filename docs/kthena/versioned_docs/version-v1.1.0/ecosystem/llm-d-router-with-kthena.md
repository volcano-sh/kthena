# llm-d Router with Kthena

This guide shows how to route requests from
[llm-d Router](https://github.com/llm-d/llm-d-router) to model servers managed by
Kthena. The integration uses labels that Kthena already adds to ModelServing
Pods, so no Kthena controller changes are required.

The examples use llm-d Router v0.10.0. Deploy the router in the same namespace
as the ModelServing workload.

## Prerequisites

- A Kubernetes cluster with Kthena installed.
- Helm 3 or later.
- For Gateway mode, Gateway API and Gateway API Inference Extension v1 CRDs,
  plus a compatible Gateway implementation.
- For P/D disaggregation, Kubernetes 1.29 or later. The example uses a native
  sidecar container, which is not enabled by default on Kubernetes 1.28.

## Kthena labels

Kthena adds the following labels to ModelServing Pods:

| Label | Use |
| --- | --- |
| `modelserving.volcano.sh/name` | Select a ModelServing workload |
| `modelserving.volcano.sh/entry` | Exclude worker Pods from routing |
| `modelserving.volcano.sh/role` | Select prefill or decode Pods |
| `modelserving.volcano.sh/group-name` | Keep P/D routing within a ServingGroup |
| `modelserving.volcano.sh/revision` | Identify a ModelServing revision |

The examples assume the ModelServing is named `my-model` and its role names are
`prefill` and `decode`. Adjust the selectors if different names are used.

Choose either standalone mode or Gateway mode for each router release.

## Standalone mode

Standalone mode deploys EPP and an Envoy proxy without requiring a Kubernetes
Gateway. Save the following as `llm-d-router-values.yaml`:

```yaml
router:
  inferencePool:
    create: false
  modelServers:
    matchLabels:
      modelserving.volcano.sh/name: my-model
      modelserving.volcano.sh/entry: "true"
    type: vllm
    protocol: http
    targetPorts:
      - number: 8000
```

Install the standalone chart:

```bash
helm upgrade --install my-model-router \
  oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
  --version v0.10.0 \
  --namespace my-model \
  --create-namespace \
  -f llm-d-router-values.yaml
```

:::note
The chart defaults reserve production-sized resources. Override
`router.epp.resources` and `router.proxy.resources` when using a small
development cluster.
:::

## Gateway mode

Install the Gateway API Inference Extension v1 CRDs if they are not already
present:

```bash
kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.5.0/v1-manifests.yaml
```

Save the following as `llm-d-router-gateway-values.yaml`:

```yaml
router:
  inferencePool:
    create: true
    failureMode: FailClose
  modelServers:
    matchLabels:
      modelserving.volcano.sh/name: my-model
      modelserving.volcano.sh/entry: "true"
    type: vllm
    protocol: http
    targetPorts:
      - number: 8000
provider:
  name: none
```

Install the Gateway chart:

```bash
helm upgrade --install my-model-router \
  oci://ghcr.io/llm-d/charts/llm-d-router-gateway \
  --version v0.10.0 \
  --namespace my-model \
  --create-namespace \
  -f llm-d-router-gateway-values.yaml
```

The chart creates an `InferencePool` that selects only Kthena entry Pods. Use
an `HTTPRoute` to attach the pool to a Gateway implementation supported by
llm-d Router. See the
[llm-d Router Gateway mode documentation](https://github.com/llm-d/llm-d-router/blob/v0.10.0/config/charts/README.md#2-gateway-mode-llm-d-router-gateway)
for provider-specific setup.

The Gateway API Inference Extension v1 failure modes are `FailOpen` and
`FailClose`.

## Prefill/decode disaggregation

Start with a ModelServing workload containing `prefill` and `decode` roles, as
described in
[Prefill-Decode Disaggregation with ModelServing](../user-guide/prefill-decode-disaggregation/modelserving-vllm-pd-disaggregation.md).
The prefill entry server continues to listen on port 8000. The decode entry Pod
exposes the llm-d sidecar on port 8000 and moves the model server to port 8200.

### Configure the decode sidecar

Add the sidecar to the decode `entryTemplate`:

```yaml
- name: decode
  replicas: 1
  entryTemplate:
    spec:
      initContainers:
        - name: routing-sidecar
          image: ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.10.0
          restartPolicy: Always
          args:
            - --port=8000
            - --model-server-port=8200
            - --kv-connector=nixlv2
            - --secure-proxy=false
          ports:
            - name: sidecar-http
              containerPort: 8000
          readinessProbe:
            httpGet:
              path: /health
              port: 8000
      containers:
        - name: model-server
          image: <model-server-image>
          # Configure the model server to listen on port 8200.
          ports:
            - name: model-server
              containerPort: 8200
          readinessProbe:
            httpGet:
              path: /health
              port: 8200
```

`restartPolicy: Always` on an init container uses Kubernetes native sidecars.
Kubernetes 1.28 clusters can only use this syntax when the `SidecarContainers`
feature gate is enabled on the control plane and every node; this guide requires
Kubernetes 1.29 or later, where the gate is enabled by default.

The `nixlv2` sidecar connector corresponds to vLLM's `NixlConnector`. Configure
the prefill and decode model servers with compatible `--kv-transfer-config`
values.

### Configure EPP for P/D routing

The EPP configuration must define the role filters, P/D scheduling profiles,
profile handler, and ServingGroup screener in one `EndpointPickerConfig`. Save
the following as `llm-d-router-pd-values.yaml`:

```yaml
router:
  inferencePool:
    create: false
  modelServers:
    matchLabels:
      modelserving.volcano.sh/name: my-model
      modelserving.volcano.sh/entry: "true"
    type: vllm
    protocol: http
    targetPorts:
      - number: 8000
  epp:
    flags:
      allow-experimental-plugins: true
    pluginsConfigFile: kthena-pd.yaml
    pluginsCustomConfig:
      kthena-pd.yaml: |
        apiVersion: llm-d.ai/v1alpha1
        kind: EndpointPickerConfig
        plugins:
          - type: disaggregatedset-rollout-screener
            name: kthena-serving-group
            parameters:
              scope:
                labelSelector: "modelserving.volcano.sh/name=my-model,modelserving.volcano.sh/entry=true"
              revisionGating:
                mode: max-role
                requireRoles:
                  values: [prefill, decode]
                revisionLabelKey: modelserving.volcano.sh/group-name
                roleLabelKey: modelserving.volcano.sh/role
          - type: label-selector-filter
            name: kthena-prefill
            parameters:
              matchExpressions:
                - key: modelserving.volcano.sh/role
                  operator: In
                  values: [prefill]
          - type: label-selector-filter
            name: kthena-decode
            parameters:
              matchExpressions:
                - key: modelserving.volcano.sh/role
                  operator: In
                  values: [decode]
          - type: approx-prefix-cache-producer
            parameters:
              autoTune: false
              blockSizeTokens: 5
              maxPrefixTokensToMatch: 1280
              lruCapacityPerServer: 31250
          - type: prefix-cache-scorer
          - type: max-score-picker
          - type: prefix-based-pd-decider
            parameters:
              nonCachedTokens: 8
              promptTokens: 0
          - type: disagg-profile-handler
            parameters:
              profiles:
                prefill: prefill
                decode: decode
              deciders:
                prefill: prefix-based-pd-decider
        schedulingProfiles:
          - name: prefill
            plugins:
              - pluginRef: kthena-prefill
              - pluginRef: max-score-picker
              - pluginRef: prefix-cache-scorer
          - name: decode
            plugins:
              - pluginRef: kthena-decode
              - pluginRef: max-score-picker
              - pluginRef: prefix-cache-scorer
```

The screener has its own Pod watch, independent of the router's endpoint
selector. Including `modelserving.volcano.sh/entry=true` in its scope prevents
worker Pods from making an incomplete ServingGroup appear routable. The
screener is experimental in llm-d Router v0.10.0, which is why the EPP flag is
required.

Install or update the standalone router with the P/D values:

```bash
helm upgrade --install my-model-router \
  oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
  --version v0.10.0 \
  --namespace my-model \
  --create-namespace \
  -f llm-d-router-pd-values.yaml
```

The example disaggregates requests with at least eight non-cached prompt
tokens. Tune `nonCachedTokens` and the scoring configuration for the workload.
The values above target standalone mode. For Gateway mode, add the same
`router.epp` block to the Gateway values and keep `router.inferencePool.create`
set to `true`.

## Verify the integration

Forward the standalone router Service and send an OpenAI-compatible request:

```bash
kubectl -n my-model port-forward service/my-model-router-epp 8081:8081

curl http://127.0.0.1:8081/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{
    "model": "my-model",
    "messages": [{
      "role": "user",
      "content": "Explain how separate prefill and decode servers cooperate during inference."
    }],
    "max_tokens": 8
  }'
```

The request model must match the name served by the model server. For P/D
inference, check the prefill and decode logs for the same request ID and confirm
that both Pods have the same `modelserving.volcano.sh/group-name` value.

## Limitations

- ServingGroup screening uses the experimental
  `disaggregatedset-rollout-screener` in llm-d Router v0.10.0.
- The decode sidecar is configured manually until Kthena provides a reusable
  ModelServing plugin.
- The integration has been verified with the llm-d inference simulator. Validate
  the selected vLLM KV connector and GPU data path before production use.
- Updating the EPP plugin ConfigMap may require restarting the EPP Deployment.
