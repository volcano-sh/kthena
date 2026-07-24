# Autoscaler Webhook

The autoscaler webhook is a Kubernetes admission controller that provides validation and mutation for AutoscalingPolicy resources in Kthena. It runs as part of the controller-manager webhook server.

## AutoscalingPolicy Validation

The validating webhook enforces the following rules for AutoscalingPolicy resources:

- Exactly one target type is set: homogeneousTarget, heterogeneousTarget, or disaggregatedTarget
- Target references use ModelServing or leave kind empty
- Target reference names are set
- Metric target values are positive
- Metric names are unique
- Homogeneous and heterogeneous targets define at least one top-level metric
- Scale up and scale down policy periods are within supported bounds

## AutoscalingPolicy Defaults

The mutating webhook applies defaults for missing behavior fields, including scale up, scale down, stable policy, and panic policy settings.

## Webhook Endpoints

- Validation: `/validate/autoscalingpolicy`
- Mutation: `/mutate/autoscalingpolicy`

## Testing

Run the autoscaler webhook tests from the repository root:

```bash
go test ./pkg/autoscaler/webhook/...
```
