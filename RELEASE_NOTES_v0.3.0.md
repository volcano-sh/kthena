# Release v0.3.0

Released: 2026-01-31

## Summary

This release introduces major networking and observability enhancements to Kthena. Key highlights include full **Gateway API support** for advanced traffic management, a comprehensive **Observability framework** with metrics and access logs, and **HPA support** for ModelServing. Additionally, it brings a new **Binpack Scale Down** strategy for cost optimization and significant improvements to the E2E testing framework.

*Note: This release includes a breaking change to the PodGroup API.*

## What's New

### Gateway API Support

**Background and Motivation**:
Previously, `ModelRoute` resources shared a global `modelName` namespace, leading to conflicts when multiple users attempted to route the same model name (e.g., `deepseek-r1`). Gateway API support resolves this by allowing independent routing spaces bound to different Gateways. It also lays the foundation for supporting the Kubernetes Gateway API Inference Extension.

**Key Capabilities**:

- **Independent Routing**: Bind `ModelRoute` to specific Gateways to isolate traffic.
- **Conflict Resolution**: Multiple `ModelRoute` resources can use the same `modelName` if bound to different Gateways.
- **Flexible Listeners**: Support for multiple listeners and protocols via Gateway API.

**Configuration**:

To enable Gateway API support:

```bash
helm install kthena ... --set networking.kthenaRouter.gatewayAPI.enabled=true
```

To bind a `ModelRoute` to a Gateway:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
spec:
  modelName: "deepseek-r1"
  parentRefs:
  - name: "my-gateway"  # Name of the Gateway
    namespace: "default"
    kind: "Gateway"
  rules:
  - targetModels:
    - modelServerName: "my-backend"
```

**Related**:

- User Guide: [Gateway API Support](docs/kthena/docs/user-guide/gateway-api-support.md)
- PRs: `e2e for gateway inference extension`, `doc for gateway api support`

### Observability Framework

**Background and Motivation**:
 Diagnosing performance issues in LLM inference (e.g., slow time-to-first-token, 5xx errors) requires deep visibility. The new observability framework provides production-grade monitoring capabilities directly from the router.

**Key Capabilities**:

- **Prometheus Metrics**: Rich metrics for requests, latency (prefill/decode), tokens, and scheduler fairness exposed on port `8080`.
- **Structured Access Logs**: Detailed per-request JSON logs for forensic analysis.
- **Debug Endpoints**: Real-time inspection of routing tables and upstream health on port `15000`.

**Configuration**:

Enable via Helm or environment variables:

```yaml
env:
- name: ACCESS_LOG_ENABLED
  value: "true"
- name: ACCESS_LOG_FORMAT
  value: "json"
```

**Related**:

- User Guide: [Router Observability](docs/kthena/docs/user-guide/router-observability.md)
- Commits: `feat(doc): Add Observability category to user guide`

### ModelServing Scale Subresource (HPA Support)

**Background and Motivation**:
To enable autoscaling of inference workloads based on metrics like QPS or GPU utilization, `ModelServing` resources now expose a standard Kubernetes scale subresource. This allows integration with Horizontal Pod Autoscalers (HPA) and use of `kubectl scale`.

**Configuration**:

```bash
# Scale manually
kubectl scale modelserving my-model --replicas=3

# Or use HPA
kubectl autoscale modelserving my-model --min=1 --max=5 --cpu-percent=80
```

**Related**:

- Commits: `Add scale subresource to modelServing` (@LiZhenCheng9527)

### Binpack Scale Down

**Background and Motivation**:
Standard scale-down removes pods based on ID. Binpack scale-down utilizes `controller.kubernetes.io/pod-deletion-cost` to selectively remove the group or role with the "lowest cost" (least important workload) first, maximizing available contiguous node capacity for large upcoming jobs.

**Related**:

- User Guide: [Binpack Scale Down](docs/kthena/docs/user-guide/binpack-scale-down.md)
- Commits: `add userguide of binpack scale down feature` (@LiZhenCheng9527)

## Breaking Changes

### PodGroup API Update

The `PodGroup` API (imported from Volcano) has changed. The field `minTaskMember` has been replaced by `subGroupSize` in Gang scheduling policies.

**Action Required**:
Update your `PodGroup` manifests or any custom resources defining Gang scheduling policies to use `subGroupSize` instead of `minTaskMember`.

```yaml
# OLD
spec:
  minTaskMember: 1

# NEW
spec:
  subGroupSize: 1
```

**Related**: `remove using minTaskMember and turn to subGroupSize`

## Other Notable Changes

### Improvements & Features

- **One-Click Deploy**: Enhanced `hack/local-up-kthena.sh` for easier deployment from source (@zhoujinyu).
- **Controller Flags**: Added `--controllers` flag to specify which controllers to start (@LiZhenCheng9527).
- **Service Naming**: Renamed services (e.g., `kthena-router`) for consistency across the platform.

### Testing & Quality

- **E2E Tests**: Major refactoring and new tests for:
  - Gateway API Integration
  - Multi-Model Routing
  - ModelRouteSubset
  - PD Disaggregation
- **CI/CD**: Improved Python linting workflows and established linting thresholds.

### Documentation

- **New Guides**: Network Topology Aware Scheduling, Binpack Scale Down, Gateway Inference Extension.
- **Automation**: Automated Helm chart documentation generation.

## Contributors

Thank you to all contributors who made this release possible:

@YaoZengzeng, @LiZhenCheng9527, @Lu Ma, @zhoujinyu, @katara-Jayprakash, @Zhonghu Xu, @liuhaiyu, @Yogesh Kumar
