import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Gateway Inference Extension Support

## Overview

[Gateway Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) provides a standardized way to expose AI/ML inference services through [Kubernetes Gateway API](https://gateway-api.sigs.k8s.io/). This guide demonstrates how to integrate Kthena-deployed models with the upstream Gateway API Inference Extension, enabling intelligent routing and traffic management for inference workloads.

The Gateway API Inference Extension extends the standard Kubernetes Gateway API with inference-specific resources:

- **InferencePool**: Manages collections of model server endpoints with automatic discovery and health monitoring
- **InferenceObjective**: Defines priority and capacity policies for inference requests  
- **Gateway Integration**: Seamless integration with popular gateway implementations including Kthena Router (native support), Envoy Gateway, Istio and Kgateway
- **Model-Aware Routing**: Advanced routing capabilities based on model names, adapters, and request characteristics
- **OpenAI API Compatibility**: Full support for OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/completions`)

## Prerequisites

- Kubernetes cluster with Kthena installed (see [Installation](../getting-started/installation.md))
- Gateway API installed (see [Gateway API](https://gateway-api.sigs.k8s.io/))
- Basic understanding of Gateway API and Gateway Inference Extension

## Getting Started

### Deploy Sample Model Server

First, deploy a model that will serve as the backend for the Gateway Inference Extension. Follow the [Quick Start](../getting-started/quick-start.md) guide to deploy a model in the `default` namespace and ensure it's in `Active` state.

After deployment, identify the labels of your model pods as these will be used to associate the InferencePool with your model instances:

```bash
# Get the model pods and their labels
kubectl get pods -n <your-namespace> -l workload.serving.volcano.sh/managed-by=workload.serving.volcano.sh --show-labels

# Example output shows labels like:
# modelserving.volcano.sh/name=demo-backend1
# modelserving.volcano.sh/group-name=demo-backend1-0
# modelserving.volcano.sh/role=leader
# workload.serving.volcano.sh/model-name=demo
# workload.serving.volcano.sh/backend-name=backend1
# workload.serving.volcano.sh/managed-by=workload.serving.volcano.sh
```

### Install the Inference Extension CRDs

Install the Gateway API Inference Extension CRDs in your cluster:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml
```

### Deploy the InferencePool and Endpoint Picker Extension

Choose one of the following options based on your gateway implementation:

<Tabs groupId="gateway-provider">
<TabItem value="kthena-router" label="Kthena Router">

Kthena Router natively supports Gateway Inference Extension and does not require the Endpoint Picker Extension. You can directly create an InferencePool resource that selects your Kthena model endpoints:

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
EOF
```

</TabItem>
<TabItem value="istio" label="Istio">

Install an InferencePool that selects from Kthena model endpoints with the appropriate labels. The Helm install command automatically installs the endpoint-picker, inferencepool along with provider specific resources.

For Istio deployment:

```bash
export GATEWAY_PROVIDER=istio
export IGW_CHART_VERSION=v1.0.1-rc.1

# Install InferencePool and Endpoint Picker pointing to your Kthena model pods
helm install kthena-demo \
  --set inferencePool.modelServers.matchLabels."workload\.serving\.volcano\.sh/model-name"=demo \
  --set provider.name=$GATEWAY_PROVIDER \
  --version $IGW_CHART_VERSION \
  oci://registry.k8s.io/gateway-api-inference-extension/charts/inferencepool
```

</TabItem>
</Tabs>

### Deploy an Inference Gateway

<Tabs groupId="gateway-provider">
<TabItem value="kthena-router" label="Kthena Router">

Kthena Router natively supports Gateway API and Gateway Inference Extension. You don't need to deploy additional gateway components, but you need to enable the Gateway API and Gateway Inference Extension flags in your Kthena Router deployment.

1. **Enable Gateway API and Gateway Inference Extension in Kthena Router**:

   You need to add the required flags to your Kthena Router deployment. You can do this by patching the deployment:

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

   Alternatively, you can edit the deployment directly:

   ```bash
   kubectl edit deployment kthena-router -n kthena-system
   ```

   Then add the following flags to the `args` section of the kthena-router container:
   ```yaml
   args:
     - --port=8080
     - --enable-webhook=true
     # ... other existing args ...
     - --enable-gateway-api=true
     - --enable-gateway-api-inference-extension=true
   ```

   Wait for the deployment to roll out:
   ```bash
   kubectl rollout status deployment/kthena-router -n kthena-system
   ```

2. **Deploy the Gateway**:

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

3. **Deploy the HTTPRoute**:

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
       namespace: kthena-system
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

</TabItem>
<TabItem value="istio" label="Istio">

Deploy the Istio-based inference gateway and routing configuration:

1. **Install Istio** (if not already installed):
   ```bash
   TAG=1.27.1

   curl -L https://istio.io/downloadIstio | ISTIO_VERSION=1.27.1 TARGET_ARCH=x86_64 sh -

   cd istio-$TAG/bin

   ./istioctl install --set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true
   ```

2. **Deploy the Gateway**:
   ```bash
   kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/raw/main/config/manifests/gateway/istio/gateway.yaml
   ```

3. **Deploy the HTTPRoute**:

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

</TabItem>
</Tabs>

### Verify Gateway Installation

Confirm that the Gateway was assigned an IP address and reports a `Programmed=True` status:

```bash
kubectl get gateway inference-gateway

# Expected output:
# NAME                CLASS     ADDRESS         PROGRAMMED   AGE
# inference-gateway   istio     <GATEWAY_IP>    True         30s
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

## Try it out

Wait until the gateway is ready and test inference through the gateway:

<Tabs groupId="gateway-provider">
<TabItem value="kthena-router" label="Kthena Router">

```bash
# Get the kthena-router IP or hostname
ROUTER_IP=$(kubectl get service kthena-router -n kthena-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
# If LoadBalancer is not available, use NodePort or port-forward
# kubectl port-forward -n kthena-system service/kthena-router 80:80
# Test the default port
curl http://${ROUTER_IP}:80/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen2.5-0.5B-Instruct",
    "prompt": "Write as if you were a critic: San Francisco",
    "max_tokens": 100,
    "temperature": 0
  }'
```

</TabItem>
<TabItem value="istio" label="Istio">

```bash
# Get the gateway IP address
IP=$(kubectl get gateway/inference-gateway -o jsonpath='{.status.addresses[0].value}')
PORT=80

# Test completions endpoint
curl -i ${IP}:${PORT}/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "Qwen2.5-0.5B-Instruct",
    "prompt": "Write as if you were a critic: San Francisco",
    "max_tokens": 100,
    "temperature": 0
  }'
```

</TabItem>
</Tabs>

## Cleanup

To clean up all resources created in this guide:

1. **Uninstall the InferencePool and model resources**:
   ```bash
   kubectl delete inferencepool kthena-demo --ignore-not-found
   kubectl delete modelbooster demo -n <your-namespace> --ignore-not-found
   ```

2. **Remove Gateway API Inference Extension CRDs**:
   ```bash
   kubectl delete -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml --ignore-not-found
   ```

3. **Clean up Gateway resources**:
   ```bash
   kubectl delete gateway inference-gateway --ignore-not-found
   kubectl delete httproute kthena-demo-route --ignore-not-found
   ```

4. **Remove Istio** (if you want to clean up everything):
   ```bash
   istioctl uninstall -y --purge
   kubectl delete ns istio-system
   ```
