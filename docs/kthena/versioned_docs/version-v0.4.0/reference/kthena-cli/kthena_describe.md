---
title: Kthena CLI
---
## kthena describe

Show detailed information about a specific resource

### Synopsis

Show detailed information about a specific resource.

You can describe templates and other kthena resources.

Examples:
  kthena describe template DeepSeek-R1-Distill-Qwen-32B
  kthena describe model-booster my-model
  kthena describe model-serving my-serving
  kthena describe autoscaling-policy my-policy

### Options

```
  -h, --help               help for describe
  -n, --namespace string   Kubernetes namespace (default: current context namespace)
```

### SEE ALSO

* [kthena](kthena.md)	 - Kthena CLI for managing AI inference workloads
* [kthena describe autoscaling-policy](kthena_describe_autoscaling-policy.md)	 - Show detailed information about an autoscaling policy
* [kthena describe model-booster](kthena_describe_model-booster.md)	 - Show detailed information about a model booster
* [kthena describe model-serving](kthena_describe_model-serving.md)	 - Show detailed information about a model serving workload
* [kthena describe template](kthena_describe_template.md)	 - Show detailed information about a template

