---
slug: gateway-api-support
title: Kthena Router Supports Gateway API and Inference Extension
authors: [YaoZengzeng]
tags: []
date: 2025-12-16
---

# Kthena Router Supports Gateway API and Inference Extension

## Introduction

As Kubernetes becomes the de facto standard for deploying AI/ML workloads, the need for standardized, interoperable traffic management APIs has become increasingly important. The [Kubernetes Gateway API](https://gateway-api.sigs.k8s.io/) represents a significant evolution from the traditional Ingress API, providing a more expressive, role-oriented, and extensible model for managing north-south traffic in Kubernetes clusters.

Building on top of Gateway API, the [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) introduces specialized resources and capabilities designed specifically for AI/ML inference workloads. This extension standardizes how inference services are exposed and routed through gateway implementations, enabling seamless integration across different gateway providers.

Kthena Router now supports both Gateway API and Gateway API Inference Extension, providing users with flexible routing options while maintaining compatibility with industry standards. This blog post explores why these APIs matter, how to enable them, and demonstrates practical usage examples.

<!-- truncate -->

## What is Gateway API and Gateway API Inference Extension?

Gateway API is a Kubernetes project that provides a standardized, role-oriented API for managing service networking. It separates concerns into distinct roles (Infrastructure Provider, Cluster Operator, and Application Developer) and supports advanced routing capabilities including cross-namespace routing, multiple protocols, and traffic splitting. Gateway API Inference Extension builds upon Gateway API to provide inference-specific capabilities for AI/ML workloads. It introduces specialized resources like InferencePool and InferenceObjective, enabling model-aware routing and OpenAI API compatibility for standardized inference service exposure and routing.

## Why Support Gateway API and Inference Extension?

There are several compelling reasons why Kthena Router should support these APIs:

### 1. Resolving Global ModelName Conflicts

In traditional routing configurations, the `modelName` field in `ModelRoute` resources is global. When multiple `ModelRoute` resources use the same `modelName`, conflicts occur, leading to undefined routing behavior. This limitation becomes problematic in multi-tenant environments where different teams or applications might want to use the same model name for different purposes.

Gateway API solves this by introducing the concept of **Gateway** resources that define independent routing spaces. Each Gateway can listen on different ports, and ModelRoutes bound to different Gateways are completely isolated, even if they share the same `modelName`. This enables:

- **Multi-tenant isolation**: Different teams can use the same model names without conflicts
- **Environment separation**: Separate routing configurations for dev, staging, and production
- **Port-based routing**: Different applications can access different backends through different ports

### 2. Industry Standard Compatibility

Gateway API is becoming the industry standard for Kubernetes service networking. By supporting Gateway API, Kthena Router:

- **Improves interoperability**: Works seamlessly with other Gateway API-compatible tools and infrastructure
- **Reduces vendor lock-in**: Users can migrate between different gateway implementations more easily
- **Leverages ecosystem**: Benefits from the broader Gateway API community and tooling

### 3. Supporting Gateway API Inference Extension

Gateway API Inference Extension provides a standardized way to expose AI/ML inference services. By supporting this extension, Kthena Router:

- **Enables standardized inference routing**: Works with InferencePool and InferenceObjective resources
- **Facilitates multi-gateway deployments**: Can work alongside other gateway implementations using the same API

### 4. Flexible Deployment Options

With Gateway API support, users can choose between:

- **Native ModelRoute/ModelServer**: Kthena's custom CRDs that provide advanced features like PD disaggregation, weighted routing, and sophisticated scheduling algorithms
- **Gateway API + Inference Extension**: Standard Kubernetes APIs that provide interoperability and compatibility with other gateway implementations

This flexibility allows users to select the approach that best fits their specific requirements and infrastructure constraints.

## Enabling Gateway API Support

### Prerequisites

Before enabling Gateway API support, ensure you have:

- Kubernetes cluster with Kthena installed (see [Installation Guide](/docs/getting-started/installation))
- Basic understanding of Kubernetes Gateway API concepts
- `kubectl` configured to access your cluster

### Configuration

Enable Gateway API support by setting the `--enable-gateway-api=true` parameter when deploying Kthena Router:

```bash
# Configure during Helm installation
helm install kthena \
  --set networking.kthenaRouter.gatewayAPI.enabled=true \
  --version v0.2.0 \
  oci://ghcr.io/volcano-sh/charts/kthena
```

Or modify the configuration in an already deployed Kthena Router:

```bash
kubectl edit deployment kthena-router -n kthena-system
```

Ensure the container arguments include `--enable-gateway-api=true`.

### Default Gateway

When Gateway API support is enabled, Kthena Router automatically creates a default Gateway with the following characteristics:

- **Name**: `default`
- **Namespace**: Same as the Kthena Router's namespace (typically `kthena-system`)
- **GatewayClass**: `kthena-router`
- **Listening Port**: Kthena Router's default service port (defaults to 8080)
- **Protocol**: HTTP

View the default Gateway:

```bash
kubectl get gateway

# Example output:
# NAME      CLASS           ADDRESS   PROGRAMMED   AGE
# default   kthena-router             True         5m
```

## Using Gateway API with Native ModelRoute/ModelServer

This example demonstrates how to use Gateway API with Kthena's native `ModelRoute` and `ModelServer` CRDs, resolving the modelName conflict problem.

### Step 1: Deploy Mock Model Servers

Deploy mock LLM services and their corresponding ModelServer resources:

```bash
# Deploy DeepSeek 1.5B mock service
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/LLM-Mock-ds1.5b.yaml

# Deploy DeepSeek 7B mock service
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/LLM-Mock-ds7b.yaml

# Create ModelServer for DeepSeek 1.5B
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/ModelServer-ds1.5b.yaml

# Create ModelServer for DeepSeek 7B
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/ModelServer-ds7b.yaml
```

Wait for the pods to be ready:

```bash
kubectl wait --for=condition=ready pod -l app=deepseek-r1-1-5b --timeout=300s
kubectl wait --for=condition=ready pod -l app=deepseek-r1-7b --timeout=300s
```

### Step 2: Create a New Gateway

Create and apply a new Gateway listening on a different port:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: kthena-gateway-8081
  namespace: default
spec:
  gatewayClassName: kthena-router
  listeners:
  - name: http
    port: 8081  # Using a different port
    protocol: HTTP
EOF

# Verify Gateway status
kubectl get gateway kthena-gateway-8081 -n default
```

**Important Note**: The newly created Gateway listens on port 8081, but you need to manually configure the Kthena Router's Service to expose this port:

```bash
# Edit the kthena-router Service
kubectl edit service kthena-router -n kthena-system
```

Add the new port in `spec.ports`:

```yaml
spec:
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
  - name: http-81  # Add new port
    port: 81
    targetPort: 8081
    protocol: TCP
```

### Step 3: Create ModelRoutes Bound to Different Gateways

Create and apply a ModelRoute bound to the default Gateway:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-default-route
  namespace: default
spec:
  modelName: "deepseek-r1"
  parentRefs:
  - name: "default"      # Bind to the default Gateway
    namespace: "kthena-system"
    kind: "Gateway"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"  # Backend ModelServer
EOF
```

Create and apply another ModelRoute using the **same modelName** but bound to the new Gateway:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-route-8081
  namespace: default
spec:
  modelName: "deepseek-r1"  # Same modelName as the default Gateway's ModelRoute
  parentRefs:
  - name: "kthena-gateway-8081"  # Bind to the new Gateway
    namespace: "default"
    kind: "Gateway"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-7b"  # Using a different backend
EOF
```

**Note**: When Gateway API is enabled, the `parentRefs` field is required. ModelRoutes without `parentRefs` will be ignored and will not route any traffic.

### Step 4: Verify the Configuration

Now you have two independent routing configurations:

1. **Default Gateway (Port 8080)**
   - ModelRoute: `deepseek-default-route`
   - ModelName: `deepseek-r1`
   - Backend: `deepseek-r1-1-5b` (DeepSeek-R1-Distill-Qwen-1.5B)

2. **New Gateway (Port 8081)**
   - ModelRoute: `deepseek-route-8081`
   - ModelName: `deepseek-r1` (same modelName)
   - Backend: `deepseek-r1-7b` (DeepSeek-R1-Distill-Qwen-7B)

Test the default Gateway (port 8080):

```bash
# Get the kthena-router IP or hostname
ROUTER_IP=$(kubectl get service kthena-router -n kthena-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# If LoadBalancer is not available, use NodePort or port-forward
# kubectl port-forward -n kthena-system service/kthena-router 80:80 81:81

# Test the default port
curl http://${ROUTER_IP}:80/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-r1",
    "prompt": "What is Kubernetes?",
    "max_tokens": 100,
    "temperature": 0
  }'

# Expected output from deepseek-r1-1-5b:
# {"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B!"}],...}
```

Test the new Gateway (port 8081):

```bash
# Test port 81
curl http://${ROUTER_IP}:81/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-r1",
    "prompt": "What is Kubernetes?",
    "max_tokens": 100,
    "temperature": 0
  }'

# Expected output from deepseek-r1-7b:
# {"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-7B!"}],...}
```

Although both requests use the same `modelName` (`deepseek-r1`), they are routed to different backend model services because they access through different ports (corresponding to different Gateways). This demonstrates how Gateway API resolves the global modelName conflict problem.

## Using Gateway API with Inference Extension

This example demonstrates how to use Gateway API Inference Extension with Kthena Router, providing a standardized way to expose and route inference services.

### Step 1: Install the Inference Extension CRDs

Install the Gateway API Inference Extension CRDs in your cluster:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml
```

### Step 2: Deploy Sample Model Server

Deploy a model that will serve as the backend for the Gateway Inference Extension. Follow the [Quick Start](/docs/getting-started/quick-start) guide to deploy a model in the `default` namespace and ensure it's in `Active` state.

After deployment, identify the labels of your model pods:

```bash
# Get the model pods and their labels
kubectl get pods -l workload.serving.volcano.sh/managed-by=workload.serving.volcano.sh --show-labels

# Example output shows labels like:
# modelserving.volcano.sh/name=demo-backend1
# modelserving.volcano.sh/group-name=demo-backend1-0
# modelserving.volcano.sh/role=leader
# workload.serving.volcano.sh/model-name=demo
# workload.serving.volcano.sh/backend-name=backend1
# workload.serving.volcano.sh/managed-by=workload.serving.volcano.sh
```

### Step 3: Deploy the InferencePool

Kthena Router natively supports Gateway Inference Extension and does not require the Endpoint Picker Extension. Create an InferencePool resource that selects your Kthena model endpoints:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: inference.networking.k8s.io/v1
kind: InferencePool
metadata:
  name: kthena-demo
spec:
  targetPorts:
    - number: 8000  # Adjust based on your model server port
  selector:
    matchLabels:
      workload.serving.volcano.sh/model-name: demo
  # Kthena Router natively supports Gateway Inference Extension and does not require the Endpoint Picker Extension.
  # It's just a placeholder for API validation.
  endpointPickerRef:
    name: kthena-demo
    port:
      number: 8000
EOF
```

### Step 4: Enable Gateway API Inference Extension in Kthena Router

Enable the Gateway API Inference Extension flag in your Kthena Router deployment:

```bash
kubectl patch deployment kthena-router -n kthena-system --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--enable-gateway-api=true"
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--enable-gateway-api-inference-extension=true"
  }
]'
```

Wait for the deployment to roll out:

```bash
kubectl rollout status deployment/kthena-router -n kthena-system
```

### Step 5: Deploy the Gateway and HTTPRoute

Create a Gateway resource that uses the `kthena-router` GatewayClass:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: inference-gateway
spec:
  gatewayClassName: kthena-router
  listeners:
  - name: http
    port: 8080
    protocol: HTTP
EOF
```

Create and apply the HTTPRoute configuration that connects the gateway to your InferencePool:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: kthena-demo-route
spec:
  parentRefs:
  - group: gateway.networking.k8s.io
    kind: Gateway
    name: inference-gateway
  rules:
  - backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: kthena-demo
    matches:
    - path:
        type: PathPrefix
        value: /
    timeouts:
      request: 300s
EOF
```

### Step 6: Verify and Test

Confirm that the Gateway was assigned an IP address and reports a `Programmed=True` status:

```bash
kubectl get gateway inference-gateway

# Expected output:
# NAME                CLASS           ADDRESS         PROGRAMMED   AGE
# inference-gateway   kthena-router   <GATEWAY_IP>    True         30s
```

Verify that all components are properly configured:

```bash
# Check Gateway status
kubectl get gateway inference-gateway -o yaml

# Check HTTPRoute status - should show Accepted=True and ResolvedRefs=True
kubectl get httproute kthena-demo-route -o yaml

# Check InferencePool status
kubectl get inferencepool kthena-demo -o yaml
```

Test inference through the gateway:

```bash
# Get the kthena-router IP or hostname
ROUTER_IP=$(kubectl get service kthena-router -n kthena-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
# If LoadBalancer is not available, use NodePort or port-forward
# kubectl port-forward -n kthena-system service/kthena-router 80:80

# Test the completions endpoint
curl http://${ROUTER_IP}:80/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen2.5-0.5B-Instruct",
    "prompt": "Write as if you were a critic: San Francisco",
    "max_tokens": 100,
    "temperature": 0
  }'
```

## Native ModelRoute/ModelServer: Advanced Features

While Gateway API and Gateway API Inference Extension provide standardized, interoperable routing capabilities, Kthena's native `ModelRoute` and `ModelServer` CRDs offer more experimental and advanced features specifically designed for AI/ML inference workloads:

### Prefill-Decode (PD) Disaggregation

Native ModelRoute/ModelServer supports PD disaggregation, where the compute-intensive prefill phase is separated from the token generation decode phase. This enables:

- **Hardware optimization**: Use specialized hardware for each phase
- **Better resource utilization**: Match workload characteristics to hardware capabilities
- **Reduced latency**: Optimize each phase independently

### Weighted-Based Routing

Native ModelRoute supports sophisticated weighted routing across multiple ModelServers, enabling:

- **Traffic splitting**: Distribute traffic across backends based on weights
- **A/B testing**: Gradually shift traffic between different model versions
- **Capacity-based routing**: Route based on backend capacity and availability

These advanced features make native ModelRoute/ModelServer ideal for production environments that require sophisticated traffic management and optimization strategies. However, Gateway API and Gateway API Inference Extension provide better interoperability and compatibility with other gateway implementations, making them suitable for multi-gateway deployments and standardized infrastructure.

## Conclusion

Kthena Router's support for Gateway API and Gateway API Inference Extension provides users with flexible routing options that balance standardization and advanced capabilities. Gateway API resolves the modelName conflict problem and enables multi-tenant isolation, while Gateway API Inference Extension provides standardized inference routing capabilities.

Users can choose between:

- **Gateway API + Inference Extension**: For standardized, interoperable routing that works across different gateway implementations
- **Native ModelRoute/ModelServer**: For advanced features like PD disaggregation, weighted routing, and sophisticated scheduling algorithms

Both approaches are fully supported and can be used together in the same cluster, providing maximum flexibility for different use cases and requirements.

For more information, please refer to the [Gateway API Support Guide](/docs/user-guide/gateway-api-support) and [Gateway Inference Extension Support Guide](/docs/user-guide/gateway-inference-extension-support). All example files referenced in this blog are available in the [kthena/examples/kthena-router](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router) directory.
