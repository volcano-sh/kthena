# Router Rate Limiting

Unlike traditional microservices that use request count or connection-based rate limiting, AI inference scenarios require **token-based rate limiting**. This is because AI requests can vary dramatically in computational cost - a single request with 10,000 tokens consumes far more GPU resources than 100 requests with 10 tokens each. Token-based limits ensure fair resource allocation based on actual computational consumption rather than simple request counts.

## Overview

Kthena Router provides powerful rate-limiting capabilities to control the traffic flow to your backend models. This is essential for preventing service overload, managing costs, and ensuring fair usage. Rate limiting is configured directly within the **ModelRoute** Custom Resource (CR).

The router supports two main types of rate limiting:
- **Local Rate Limiting**: Enforces limits on a per-router-instance basis. It\'s simple to configure and effective for basic load protection.
- **Global Rate Limiting**: Enforces a shared limit across all router instances, using a central store like Redis. This is ideal for providing consistent limits in a scaled-out environment.

Limits are based on the number of input/output tokens over a specific time window (second, minute, hour, day, or month).

## Preparation

Before diving into the rate-limiting configurations, let's set up the environment. All the configuration examples in this document can be found in the [examples/kthena-router](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router) directory of the Kthena repository.

### Prerequisites

- A running Kubernetes cluster with Kthena installed.
- For global rate limiting, a running Redis instance is required. You can deploy one using the [redis-standalone.yaml](../assets/examples/redis/redis-standalone.yaml) example.
- Basic understanding of router CRDs (ModelServer and ModelRoute).

### Getting Started

1.  Deploy a mock LLM inference engine, such as [LLM-Mock-ds1.5b.yaml](../assets/examples/kthena-router/LLM-Mock-ds1.5b.yaml), if you don't have a real GPU/NPU environment.
2.  Deploy the corresponding ModelServer, [ModelServer-ds1.5b.yaml](../assets/examples/kthena-router/ModelServer-ds1.5b.yaml).
3.  All rate-limiting examples in this guide use this mock service.

## Rate Limiting Scenarios

### 1. Local Rate Limiting

**Scenario**: Protect a model from being overwhelmed by limiting the number of tokens it can process per minute from each router pod.

**Traffic Processing**: The router inspects the number of tokens in each request. If the cumulative number of input or output tokens within a minute exceeds the defined limits, the router will reject the request with an `HTTP 429 Too Many Requests` error. This limit is tracked independently by each router pod.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-rate-limit
  namespace: default
spec:
  modelName: "deepseek-r1-with-rate-limit"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"
  # This configuration applies to all rules in this ModelRoute
  # - 10 input tokens per minute to be convenient to test
  rateLimit:
    inputTokensPerUnit: 10
    outputTokensPerUnit: 5000
    unit: minute
```

**Flow Description**:
1.  A request for the model `deepseek-r1-with-rate-limit` arrives at the router.
2.  The router checks its local counters for the token limits defined in the `rateLimit` policy.
3.  If the `inputTokensPerUnit` or `outputTokensPerUnit` limit is not exceeded, the request is forwarded to the `deepseek-r1-1-5b` ModelServer.
4.  If either limit is exceeded, the router immediately returns an `HTTP 429` status code.

**Try it out**:
To test this, you can set a low limit (e.g., `inputTokensPerUnit: 10`) and send multiple requests.

```bash
# 1. Apply the ModelRoute yaml above
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/ModelRouteWithRateLimit.yaml

# 2. Scale down the replicas of the router to 1 to demonstrate local rate limiting
kubectl scale deployment kthena-router -n kthena-system --replicas=1

# 3. Get public ip of the router pod
export ROUTER_IP=$(kubectl get svc kthena-router -n kthena-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# 4. Request router three times, the final request will be rejected
export MODEL="deepseek-r1-with-rate-limit"

curl http://$ROUTER_IP/v1/completions \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"prompt\": \"San Francisco is a\",
        \"temperature\": 0
    }"
# Expected output for rejected request:
"input token rate limit exceeded"

# 5. Clean up
kubectl delete -f https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelRouteWithRateLimit.yaml
```

### 2. Global Rate Limiting

**Scenario**: Enforce a consistent, cluster-wide rate limit for a specific model route, ensuring that the total traffic from all users and all router pods does not exceed a global threshold.

**Traffic Processing**: This works similarly to local rate limiting, but instead of using local in-memory counters, the router uses a Redis instance to share and synchronize the token counts across all router pods. This ensures the rate limit is applied globally and accurately, regardless of how many router replicas are running.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-global-rate-limit
  namespace: default
spec:
  modelName: "deepseek-r1-with-global-rate-limit"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"
  # This configuration applies to all rules in this ModelRoute
  # - 10 input tokens per minute to be convenient to test
  rateLimit:
    inputTokensPerUnit: 10
    outputTokensPerUnit: 5000
    unit: minute
    global:
      redis:
        address: "redis-server.kthena-system.svc.cluster.local:6379"
```

**Flow Description**:
1.  A request for the model `deepseek-r1-with-global-rate-limit` arrives at any router pod.
2.  The router connects to the Redis server specified in the `global.redis.address` field.
3.  It atomically checks and increments the token counters stored in Redis for this route.
4.  If the global limit is not exceeded, the request is forwarded.
5.  If the limit is exceeded, the router returns an `HTTP 429` error. All other router pods will now also enforce this limit until the time window resets.

**NOTE**: Before applying this configuration, ensure you have a Redis service running and accessible at the specified address (`redis-server.kthena-system.svc.cluster.local:6379`). You can use the provided [redis-standalone.yaml](../assets/examples/redis/redis-standalone.yaml) to deploy one. And make sure you have deployed multiple router pods.

**Try it out**:
The test process is similar to local rate limiting. Even if your requests are handled by different router pods, the global limit will be consistently enforced.

```bash
# 1. Deploy Redis service
kubectl apply -f https://github.com/volcano-sh/kthena/blob/main/examples/redis/redis-standalone.yaml

# 2. Apply the ModelRoute yaml above
kubectl apply -f https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelRouteWithGlobalRateLimit.yaml

# 3. Scale up the replicas of the router to 3 to demonstrate global rate limiting
kubectl scale deployment kthena-router -n kthena-system --replicas=3

# 4. Get public ip of the router pod
export ROUTER_IP=$(kubectl get svc kthena-router -n kthena-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# 5. Request router three times, the final request will be rejected
export MODEL="deepseek-r1-with-global-rate-limit"

curl http://$ROUTER_IP/v1/completions \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"prompt\": \"San Francisco is a\",
        \"temperature\": 0
    }"
# Expected output for rejected request:
"input token rate limit exceeded"

# 6. Clean up
kubectl delete -f https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelRouteWithGlobalRateLimit.yaml
```

By leveraging local and global rate limiting, Kthena gives you fine-grained control over your AI service traffic, enabling robust, scalable, and cost-effective model deployments.
