# Autoscaler Features

## Overview

Kthena Autoscaler dynamically adjusts serving instances based on real-time load through two configuration modes:

- **Homogeneous Target**: Manages a group of serving instances with identical configurations, ensuring stable performance while optimizing resource utilization.
- **Heterogeneous Target**: Optimizes across multiple instance types with different resource requirements and capabilities, achieving cost-efficiency through intelligent scheduling algorithms.

Both modes rely on the same underlying autoscaling mechanisms but differ in how they target and manage resources.

## Configuration

Kthena Autoscaler is configured through two custom resources: `AutoscalingPolicy` and `AutoscalingPolicyBinding`.

### Main Configuration Parameters

#### AutoscalingPolicy Configuration

The `AutoscalingPolicy` resource defines the core autoscaling strategy and behavior parameters. It includes the following key components:

##### Metrics
- **metricName**: The name of the metric to monitor (e.g., `kthena:num_requests_waiting`)
- **targetValue**: The target value for the specified metric, which serves as the scaling threshold
  - Example: Setting `targetValue: 10.0` for `kthena:num_requests_waiting` means the autoscaler will aim to maintain no more than 10 waiting requests per instance

##### TolerancePercent
- **Description**: Defines the tolerance range around the target value before scaling actions are triggered
- **Purpose**: Prevents frequent scaling (thrashing) due to minor fluctuations in metrics
- **Usage**: For example, with `tolerancePercent: 10` and a target value of 10.0, scaling will only occur if the actual metric value exceeds 11.0 (target + 10%) or falls below 9.0 (target - 10%)

##### Behavior
Controls detailed scaling behavior for both scale-up and scale-down operations:

###### ScaleUp
Defines how the system responds to increased load:

- **PanicPolicy**: Handles sudden, significant increases in load
  - **PanicThresholdPercent**: The percentage above target that triggers panic mode (e.g., `panicThresholdPercent: 150` triggers when metrics reach 150% of target)
  - **PanicModeHold**: Duration to remain in panic mode (e.g., `panicModeHold: 5m` keeps panic mode active for 5 minutes)
  - **Purpose**: Accelerates scaling during sudden traffic spikes to prevent service degradation

- **StablePolicy**: Handles gradual increases in load
  - **StabilizationWindow**: Time window to observe metrics before making scaling decisions (e.g., `stabilizationWindow: 1m` waits 1 minute of sustained high load before scaling)
  - **Period**: Interval between scaling evaluations (e.g., `period: 30s` checks conditions every 30 seconds)
  - **Purpose**: Ensures scaling decisions are based on stable load patterns rather than transient spikes

###### ScaleDown
Defines how the system responds to decreased load:

- **StabilizationWindow**: Longer time window to observe decreased load before scaling down (e.g., `stabilizationWindow: 5m` requires 5 minutes of sustained low load)
  - **Rationale**: Typically set longer than scale-up to ensure system stability and avoid premature capacity reduction
- **Period**: Interval between scale-down evaluations (e.g., `period: 1m` checks conditions every minute)

These configuration parameters work together to create a responsive yet stable autoscaling system that balances resource utilization with performance requirements.

#### AutoscalingPolicyBinding Configuration

The `AutoscalingPolicyBinding` resource connects autoscaling policies to target resources and specifies scaling boundaries. It supports two distinct scaling modes, each with its own parameter set:

##### Core Configuration Structure

```yaml
spec:
  # Reference to the autoscaling policy
  policyRef:
    name: your-autoscaling-policy-name
  
  # Select EITHER homogeneousTarget OR heterogeneousTarget mode, not both
  homogeneousTarget:
    # Homogeneous Target mode configuration
  heterogeneousTarget:
    # Heterogeneous Target mode configuration
```

##### Homogeneous Target Mode

Configures autoscaling for a single instance type:

- **target**:
  - **targetRef**: References the target serving instance
    - **name**: The name of the target resource to scale
  - **additionalMatchLabels**: Optional set of labels to further refine target resource selection
  - **metricEndpoint**: Optional endpoint configuration for custom metric collection
    - **uri**: Path to the metrics endpoint on the target pods (default: "/metrics")
    - **port**: Port number where metrics are exposed on the target pods (default: 8100)
- **minReplicas**: Minimum number of instances to maintain, ensuring baseline availability
  - Must be greater than or equal to 1
  - Sets a floor on scaling operations to prevent scaling down below this threshold
- **maxReplicas**: Maximum number of instances allowed, controlling resource consumption
  - Must be greater than or equal to 1
  - Sets a ceiling on scaling operations to prevent excessive resource allocation

##### Heterogeneous Target Mode

Configures autoscaling across multiple instance types with different capabilities and costs:

- **costExpansionRatePercent**: Defines the maximum acceptable cost increase percentage (default: 200)
  - When scaling, the algorithm will consider instance combinations that don't exceed the base cost plus this percentage
  - Higher values allow more flexibility in instance selection for better performance
  - Lower values prioritize strict cost control
- **params**: Array of configuration parameters for each instance type in the optimizer group (at least 1 is required):
  - **target**:
    - **targetRef**: References the specific instance type
      - **name**: The name of this instance type resource
    - **additionalMatchLabels**: Optional set of labels to refine selection for this instance type
    - **metricEndpoint**: Optional endpoint configuration for custom metric collection
      - **uri**: Path to the metrics endpoint on the target pods (default: "/metrics")
      - **port**: Port number where metrics are exposed on the target pods (default: 8100)
  - **minReplicas**: Minimum number of instances for this specific type
    - Ensures availability of this instance type regardless of load conditions
    - Support 0
  - **maxReplicas**: Maximum number of instances for this specific type
    - Caps resource allocation for this particular instance type
  - **cost**: Relative or actual cost metric for this instance type
    - Used by the optimization algorithm to balance performance and cost
    - Higher values represent more expensive instance types

The heterogeneous mode's optimization algorithm automatically determines the optimal combination of instance types to balance performance requirements against cost constraints, always respecting the defined minReplicas and maxReplicas boundaries for each instance type.

### Configuration Example

#### Homogeneous Target Example

The following configuration demonstrates scaling configuration for a single instance type:

```yaml
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
  homogeneousTarget:
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

**Behavior Explanation:**
- When request volume exceeds the threshold, the autoscaler will automatically scale up based on the `kthena:num_requests_waiting` metric, maintaining instance count between 2-10
- A 10% tolerance range is configured to avoid frequent scaling
- When load increases sharply beyond 150% of the target value, it will enter panic mode for 5 minutes to accelerate scaling
- In stable mode, scaling decisions are executed after a 1-minute stabilization window with a 30-second period
- Scale-down decisions are executed after a 5-minute stabilization window with a 1-minute period to ensure stable load reduction before scaling down
- Custom metric endpoint is configured to collect metrics from "/custom-metrics" endpoint on port 9090 instead of using the default values ("/metrics" on port 8100)

#### Heterogeneous Target Example

The following configuration demonstrates optimizer configuration across multiple instance types:

```yaml
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
  heterogeneousTarget:
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

**Behavior Explanation:**
- This configuration manages two different types of service instances: high-performance GPU instances and lower-cost CPU instances
- A 20% cost expansion rate is set, and the autoscaler will calculate the optimal instance combination based on this
- GPU instances (cost: 100) have minimum/maximum instance counts of 1/5 respectively
- CPU instances (cost: 30) have minimum/maximum instance counts of 2/8 respectively
- The autoscaler will prioritize scaling lower-cost CPU instances, only scaling GPU instances when load continues to grow or when reaching CPU instance limits
- During scale-down, higher-cost GPU instances are reduced first to ensure cost-effectiveness in resource usage

## Monitoring and Verification

This section describes how to verify that your autoscaling configurations are working correctly.

### Verifying Configuration Success

#### 1. Check Custom Resources

After applying your configuration, verify that the custom resources are created successfully:

```bash
# Check AutoscalingPolicy
kubectl get autoscalingpolicies.workload.serving.volcano.sh

# Check AutoscalingPolicyBinding
kubectl get autoscalingpolicybindings.workload.serving.volcano.sh
```

#### 2. Observe Scaling Events

Monitor the events generated by the autoscaler controller:

```bash
kubectl describe autoscalingpolicybindings.workload.serving.volcano.sh <binding-name>
```

Look for events that indicate scaling decisions and actions.

#### 3. Verify Target Instance Count Changes

For homogeneous scaling, check if the target instance's replica count is being adjusted according to the policy:

```bash
kubectl get modelservers.networking.serving.volcano.sh <target-name> -o jsonpath='{.spec.replicas}'
```

#### 4. Check Metrics Collection

Verify that metrics are being collected correctly by checking the autoscaler logs:

```bash
kubectl logs -n <namespace> -l app=kthena-autoscaler -c autoscaler
```

### Key Metrics to Monitor

Monitor these critical metrics to assess the effectiveness of your autoscaling configuration:

- **Current vs. Target Metric Values**: Ensure the actual metrics are approaching the target values you configured
- **Instance Count History**: Verify that instance counts are adjusting appropriately to load changes
- **Scaling Frequency**: Check if scaling events are happening too frequently (thrashing) or not frequently enough
- **Panic Mode Activations**: Monitor how often panic mode is triggered

### Debugging Common Issues

If your autoscaling configuration doesn't behave as expected:

1. **Check Metric Availability**: Ensure the metrics you're using are properly collected and available
2. **Verify Policy Binding**: Confirm that the AutoscalingPolicyBinding correctly references the target resource
3. **Inspect Controller Logs**: Look for error messages or warnings in the autoscaler controller logs
4. **Review Resource Limits**: Ensure that minReplicas and maxReplicas values are appropriately set
5. **Test with Different Loads**: Gradually increase or decrease the load to observe scaling behavior

By following these verification steps, you can ensure that your homogeneous and heterogeneous autoscaling configurations are working correctly and optimizing your workload's resource usage efficiently.
