# Gateway API Support

## Overview

Kthena Router supports [Kubernetes Gateway API](https://gateway-api.sigs.k8s.io/), providing more flexible traffic management and routing capabilities for model inference services. This guide explains why Gateway API support is needed and how to use Gateway API in Kthena Router.

## Why Gateway API Support?

There are two main reasons for introducing Gateway API support in Kthena Router:

### 1. Resolving Global ModelName Conflicts in ModelRoute

In traditional routing configurations, the `modelName` field in `ModelRoute` resources is global. When multiple `ModelRoute` resources use the same `modelName`, conflicts occur, leading to undefined routing behavior.

For example, the following two ModelRoute resources would conflict:

```yaml
# First ModelRoute
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-service-1
  namespace: default
spec:
  modelName: "deepseek-r1"  # Same modelName
  rules:
  - targetModels:
    - modelServerName: "deepseek-backend-1"

---
# Second ModelRoute
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-service-2
  namespace: default
spec:
  modelName: "deepseek-r1"  # Same modelName, conflicts!
  rules:
  - targetModels:
    - modelServerName: "deepseek-backend-2"
```

By using Gateway API, you can bind different `ModelRoute` resources to different `Gateway` resources. Even if they use the same `modelName`, there will be no conflicts. Each Gateway can listen on different ports, creating independent routing spaces.

### 2. Supporting Gateway API Inference Extension

Kthena Router will support [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/), a Kubernetes Gateway API extension standard designed for AI/ML inference services. Gateway API Inference Extension depends on the foundational features of Gateway API, so Kthena Router needs to support Gateway API first.

For more information about Gateway API Inference Extension, please refer to [Gateway Inference Extension Support](./gateway-inference-extension-support.md).

## Prerequisites

- Kubernetes cluster with Kthena installed (see [Installation Guide](../getting-started/installation.md))
- Kthena Router deployed with Gateway API support enabled
- Basic understanding of Kubernetes Gateway API concepts
- `kubectl` configured to access your cluster

**Note**: All example files referenced in this guide are available in the [kthena/examples/kthena-router](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router) directory.

## Gateway API Architecture

![Gateway API Architecture](../assets/diagrams/gateway-api-arch.svg)

In Kthena Router, Gateway API works as follows:

1. **GatewayClass**: Kthena Router automatically creates a GatewayClass named `kthena-router`
2. **Gateway**: Defines listening ports and protocols, serving as the traffic entry point
3. **ModelRoute**: Binds to specific Gateways through the `parentRefs` field
4. **Route Isolation**: ModelRoutes on different Gateways are completely isolated, even if they share the same modelName

## Enabling Gateway API Support

### 1. Configure Kthena Router

When deploying Kthena Router, enable Gateway API support by setting the `--enable-gateway-api=true` parameter:

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

### 2. Default Gateway

When Gateway API support is enabled, Kthena Router automatically creates a default Gateway with the following characteristics:

- Name: `default`
- Namespace: Same as the Kthena Router's namespace
- GatewayClass: `kthena-router`
- Listening Port: Kthena Router's default service port (defaults to 8080)
- Protocol: HTTP

View the default Gateway:

```bash
kubectl get gateway

# Example output:
# NAME      CLASS           ADDRESS   PROGRAMMED   AGE
# default   kthena-router             True         5m
```

### 3. Binding ModelRoute to the Default Gateway

You can bind a ModelRoute to the default Gateway as shown below:

```yaml
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
```

Apply the ModelRoute:

```bash
kubectl apply -f deepseek-default-route.yaml
```

**Note**: When Gateway API is enabled, the `parentRefs` field is required. ModelRoutes without `parentRefs` will be ignored and will not route any traffic.

## Use Case: Creating Independent Gateways and ModelRoutes

This example demonstrates how Gateway API resolves the modelName conflict problem. We will:

1. Deploy two different model backends (DeepSeek 1.5B and DeepSeek 7B)
2. Create two Gateways listening on different ports (8080 and 8081)
3. Create two ModelRoutes with the **same modelName** but bound to different Gateways
4. Show that requests are correctly routed to different backends based on the Gateway (port) they use

This proves that with Gateway API, multiple ModelRoutes can safely use the same modelName without conflicts.

### Step 1: Deploy Mock Model Servers

First, deploy mock LLM services and their corresponding ModelServer resources. We'll use the DeepSeek-R1-Distill-Qwen-1.5B model for the default Gateway and the DeepSeek-R1-Distill-Qwen-7B model for the new Gateway.

Deploy the mock LLM services:

```bash
# Deploy DeepSeek 1.5B mock service (for default Gateway)
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/LLM-Mock-ds1.5b.yaml

# Deploy DeepSeek 7B mock service (for new Gateway)
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/LLM-Mock-ds7b.yaml
```

Create the corresponding ModelServer resources:

```bash
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

Create a new Gateway listening on a different port:

```yaml
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
```

Apply the configuration:

```bash
kubectl apply -f gateway-8081.yaml

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
    port: 8080
    targetPort: 8080
    protocol: TCP
  - name: http-8081  # Add new port
    port: 8081
    targetPort: 8081
    protocol: TCP
```

Alternatively, create a new Service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: kthena-router-8081
  namespace: kthena-system
spec:
  selector:
    app: kthena-router
  ports:
  - name: http-8081
    port: 8081
    targetPort: 8081
    protocol: TCP
  type: LoadBalancer  # Or ClusterIP / NodePort
```

### Step 3: Create a ModelRoute Bound to the New Gateway

Now, create a ModelRoute that uses the same `modelName` as before but binds to the new Gateway:

```yaml
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
```

Apply the configuration:

```bash
kubectl apply -f deepseek-route-8081.yaml
```

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
# kubectl port-forward -n kthena-system service/kthena-router 8080:8080 8081:8081

# Test the default port
curl http://${ROUTER_IP}:8080/v1/completions \
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
# Test port 8081
curl http://${ROUTER_IP}:8081/v1/completions \
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

## Cleanup

Delete the resources created in the examples:

```bash
# Delete ModelRoutes
kubectl delete modelroute deepseek-default-route -n default
kubectl delete modelroute deepseek-route-8081 -n default

# Delete Gateway
kubectl delete gateway kthena-gateway-8081 -n default

# Delete ModelServers
kubectl delete modelserver deepseek-r1-1-5b -n default
kubectl delete modelserver deepseek-r1-7b -n default

# Delete mock LLM deployments
kubectl delete deployment deepseek-r1-1-5b -n default
kubectl delete deployment deepseek-r1-7b -n default
```
