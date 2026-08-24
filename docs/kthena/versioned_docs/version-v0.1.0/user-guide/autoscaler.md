# Kthena Autoscaler

## Overview

Kthena Autoscaler dynamically adjusts serving instances based on real-time workload metrics, ensuring optimal performance and resource utilization. The autoscaler supports two distinct configuration modes:

- **Scaling Configuration**: Manages a homogeneous group of serving instances with identical configurations, ensuring stable performance while optimizing resource utilization.
- **Optimizer Configuration**: Optimizes across heterogeneous instance types with different resource requirements and capabilities, achieving cost-efficiency through intelligent scheduling algorithms.

Both modes leverage the same core autoscaling mechanisms but differ in their resource targeting and management approaches.

## Configuration Guide

The autoscaler operates through two primary custom resources:

- **[`AutoscalingPolicy`](../reference/crd/workload.serving.volcano.sh.md#autoscalingpolicy)**: Defines the core autoscaling strategy, metrics, and behavior parameters
- **[`AutoscalingPolicyBinding`](../reference/crd/workload.serving.volcano.sh.md#autoscalingpolicybinding)**: Connects policies to target resources and specifies scaling boundaries

### AutoscalingPolicy Configuration

The [`AutoscalingPolicy`](../reference/crd/workload.serving.volcano.sh.md#autoscalingpolicy) resource defines the core autoscaling strategy and behavior parameters.

#### Core Components

##### Metrics Configuration
- **metricName**: Name of the metric to monitor (e.g., `kthena:num_requests_waiting`)
- **targetValue**: Target value for the specified metric, serving as the scaling threshold
  - *Example*: Setting `targetValue: 10.0` for `kthena:num_requests_waiting` means the autoscaler will aim to maintain no more than 10 waiting requests per instance

##### Tolerance Configuration
- **tolerancePercent**: Defines the tolerance range around the target value before scaling actions are triggered
- **Purpose**: Prevents frequent scaling (thrashing) due to minor metric fluctuations
- *Example*: With `tolerancePercent: 10` and a target value of 10.0, scaling occurs only if the actual metric value exceeds 11.0 (target + 10%) or falls below 9.0 (target - 10%)

##### Behavior Configuration

Controls detailed scaling behavior for both **scale-up** and **scale-down** operations:

| Policy | Parameter | Description | Example | Purpose/Rationale |
|--------|-----------|-------------|---------|-------------------|
| Scale-Up (Panic) | `panicThresholdPercent` | Percentage above target that triggers panic mode | `150` triggers when metrics reach 150% of target | Accelerates scaling during sudden traffic spikes to prevent service degradation |
| Scale-Up (Panic) | `panicModeHold` | Duration to remain in panic mode | `5m` keeps panic mode active for 5 minutes | Ensures panic mode persists long enough to handle spike |
| Scale-Up (Stable) | `stabilizationWindow` | Time window to observe metrics before making scaling decisions | `1m` waits 1 minute of sustained high load before scaling | Ensures scaling decisions are based on stable load patterns rather than transient spikes |
| Scale-Up (Stable) | `period` | Interval between scaling evaluations | `30s` checks conditions every 30 seconds | Regular assessment of load conditions |
| Scale-Down | `stabilizationWindow` | Longer time window to observe decreased load before scaling down | `5m` requires 5 minutes of sustained low load | Typically set longer than scale-up to ensure system stability and avoid premature capacity reduction |
| Scale-Down | `period` | Interval between scale-down evaluations | `1m` checks conditions every minute | Regular assessment of load conditions |

These configuration parameters work together to create a responsive yet stable autoscaling system that balances resource utilization with performance requirements.

### AutoscalingPolicyBinding Configuration

The [`AutoscalingPolicyBinding`](../reference/crd/workload.serving.volcano.sh.md#autoscalingpolicybinding) resource connects autoscaling policies to target resources and specifies scaling boundaries. It supports two distinct scaling modes:

#### Configuration Structure

```yaml
spec:
  # Reference to the autoscaling policy
  policyRef:
    name: your-autoscaling-policy-name
  
  # Select EITHER scalingConfiguration OR optimizerConfiguration mode, not both
  scalingConfiguration:
    # Scaling Configuration mode parameters
  optimizerConfiguration:
    # Optimizer Configuration mode parameters
```

#### Scaling Configuration Mode

Configures autoscaling for a single instance type (homogeneous scaling):

**Target Configuration**:
- **targetRef**: References the target serving instance
  - **name**: Name of the target resource to scale
- **additionalMatchLabels**: Optional labels to refine target resource selection
- **metricEndpoint**: Optional custom metric collection endpoint
  - **uri**: Path to metrics endpoint on target pods (default: "/metrics")
  - **port**: Port number where metrics are exposed (default: 8100)

**Scaling Boundaries**:
- **minReplicas**: Minimum number of instances to maintain (≥ 1)
  - Ensures baseline availability and prevents scaling below this threshold
- **maxReplicas**: Maximum number of instances allowed (≥ 1)
  - Controls resource consumption and prevents excessive allocation

#### Optimizer Configuration Mode

Configures autoscaling across multiple instance types with different capabilities and costs (heterogeneous scaling):

**Cost Optimization**:
- **costExpansionRatePercent**: Maximum acceptable cost increase percentage (default: 200)
  - Algorithm considers instance combinations within base cost plus this percentage
  - Higher values: More flexibility for performance optimization
  - Lower values: Strict cost control

**Instance Type Parameters** (array, at least 1 required):
- **target**: Instance type configuration
  - **targetRef**: References the specific instance type
    - **name**: Name of this instance type resource
  - **additionalMatchLabels**: Optional labels to refine instance selection
  - **metricEndpoint**: Optional custom metric collection endpoint
- **minReplicas**: Minimum instances for this type (supports 0)
  - Ensures availability of specific instance types
- **maxReplicas**: Maximum instances for this type
  - Caps resource allocation per instance type
- **cost**: Relative or actual cost metric
  - Used by optimization algorithm to balance performance and cost
  - Higher values represent more expensive instance types

The optimization algorithm automatically determines the optimal combination of instance types to balance performance against cost constraints, while respecting the defined boundaries for each instance type.

### Configuration Examples

#### Scaling Configuration Example

This example demonstrates homogeneous scaling for a single instance type:

```yaml showLineNumbers
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: scaling-policy
spec:
  metrics:
  - metricName: kthena:num_requests_waiting
    targetValue: 10.0
  tolerancePercent: 10
  behavior:
    scaleUp:
      panicPolicy:
        panicThresholdPercent: 150
        panicModeHold: 5m
      stablePolicy:
        stabilizationWindow: 1m
        period: 30s
    scaleDown:
      stabilizationWindow: 5m
      period: 1m
---
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: scaling-binding
spec:
  policyRef:
    name: scaling-policy
  scalingConfiguration:
    target:
      targetRef:
        name: example-model-serving
      # Optional: Customize metric collection endpoint
      metricEndpoint:
        uri: "/custom-metrics"  # Custom metric path
        port: 9090               # Custom metric port
    minReplicas: 2
    maxReplicas: 10
```

**Key Behavior Characteristics:**
- **Metric Target**: Maintains no more than 10 waiting requests per instance
- **Scaling Range**: Operates between 2-10 replicas
- **Tolerance**: 10% buffer prevents frequent scaling for minor fluctuations
- **Panic Mode**: Triggers accelerated scaling when load exceeds 150% of target, remaining active for 5 minutes
- **Stable Scaling**: 1-minute observation window with 30-second evaluation intervals
- **Conservative Scale-down**: 5-minute stabilization window ensures load reduction is sustained
- **Custom Metrics**: Collects from `/custom-metrics` on port 9090 instead of defaults

#### Optimizer Configuration Example

This example demonstrates heterogeneous scaling across multiple instance types with cost optimization:

```yaml showLineNumbers
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: optimizer-policy
spec:
  metrics:
  - metricName: kthena:num_requests_waiting
    targetValue: 10.0
  tolerancePercent: 10
  behavior:
    scaleUp:
      panicPolicy:
        panicThresholdPercent: 150
        panicModeHold: 5m
      stablePolicy:
        stabilizationWindow: 1m
        period: 30s
    scaleDown:
      stabilizationWindow: 5m
      period: 1m
---
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: optimizer-binding
spec:
  policyRef:
    name: optimizer-policy
  optimizerConfiguration:
    costExpansionRatePercent: 20
    params:
    - target:
        targetRef:
          name: gpu-serving-instance
      minReplicas: 1
      maxReplicas: 5
      cost: 100
    - target:
        targetRef:
          name: cpu-serving-instance
      minReplicas: 2
      maxReplicas: 8
      cost: 30
```

**Optimization Strategy:**
- **Cost Control**: 20% maximum cost expansion allows flexible instance selection
- **Instance Types**: Manages high-performance GPU (cost: 100) and cost-effective CPU (cost: 30) instances
- **GPU Boundaries**: 1-5 replicas for high-performance workloads
- **CPU Boundaries**: 2-8 replicas for general workloads
- **Scaling Priority**: Preferentially scales cheaper CPU instances first
- **Cost Optimization**: During scale-down, reduces expensive GPU instances first
- **Performance Assurance**: Maintains minimum GPU instances for baseline high-performance capability

## Monitoring and Verification

This section describes how to monitor and verify that your autoscaling configurations are working correctly.

### Verification Steps

#### 1. Check Custom Resource Status

After applying your configuration, verify that the custom resources are created successfully:

```bash
# Check AutoscalingPolicy status
kubectl get autoscalingpolicies.workload.serving.volcano.sh

# Check AutoscalingPolicyBinding status
kubectl get autoscalingpolicybindings.workload.serving.volcano.sh
```

#### 2. Monitor Scaling Events

Monitor the events generated by the autoscaler controller:

```bash
kubectl describe autoscalingpolicybindings.workload.serving.volcano.sh <binding-name>
```

Look for events that indicate scaling decisions, metric collection status, and any errors.

#### 3. Verify Instance Count Changes

For scaling configurations, check if the target instance's replica count is being adjusted:

```bash
# For ModelServer instances
kubectl get modelservers.networking.serving.volcano.sh <target-name> -o jsonpath='{.spec.replicas}'

# For ModelBooster instances - check current replicas per backend
kubectl get modelboosters.workload.serving.volcano.sh <target-name> -o jsonpath='{.status.backendStatuses[*].replicas}'

# For detailed backend status including replica counts
kubectl get modelboosters.workload.serving.volcano.sh <target-name> -o jsonpath='{range .status.backendStatuses[*]}{.name}: {.replicas}{"\n"}{end}'
```

#### 4. Check Metrics Collection

Verify that metrics are being collected correctly by examining autoscaler logs:

```bash
kubectl logs -n <namespace> -l app=kthena-autoscaler -c autoscaler
```

### Key Performance Indicators

Monitor these critical metrics to assess autoscaling effectiveness:

- **Metric Performance**: Compare current metric values against configured targets
- **Replica Count Trends**: Track instance count adjustments in response to load changes
- **Scaling Frequency**: Identify excessive scaling (thrashing) or insufficient responsiveness
- **Panic Mode Usage**: Monitor how often panic mode activates during traffic spikes

### Troubleshooting Guide

If your autoscaling configuration doesn't behave as expected:

1. **Verify Metric Availability**: Ensure target metrics are properly collected and exposed
2. **Check Policy Binding**: Confirm [`AutoscalingPolicyBinding`](../reference/crd/workload.serving.volcano.sh.md#autoscalingpolicybinding) correctly references both policy and target resources
3. **Examine Controller Logs**: Look for error messages or warnings in autoscaler controller logs
4. **Review Scaling Boundaries**: Ensure `minReplicas` and `maxReplicas` values are appropriately set
5. **Test Load Patterns**: Gradually increase or decrease load to observe scaling behavior across different conditions
6. **Check Resource Availability**: Verify cluster has sufficient resources for scaling operations

By following these monitoring and verification practices, you can ensure your autoscaling configurations are working correctly and optimizing workload resource usage efficiently.

## Summary

Kthena Autoscaler provides powerful, flexible autoscaling capabilities for your serving workloads:

- **Dual Modes**: Choose between homogeneous scaling (single instance type) or heterogeneous optimization (multiple instance types)
- **Precise Control**: Fine-tune scaling behavior with panic thresholds, stabilization windows, and tolerance ranges
- **Cost Optimization**: Automatically balance performance and cost across different instance types

For more advanced configurations and use cases, refer to the [Kthena CLI reference](../reference/cli/kthena.md) and [CRD documentation](../reference/crd/workload.serving.volcano.sh.md).
