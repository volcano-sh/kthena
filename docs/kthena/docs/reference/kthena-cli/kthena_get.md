---
title: Kthena CLI
---
## kthena get

Display one or many resources

### Synopsis

Display one or many resources.

You can get templates, models, model-servings, and autoscaling policies.

Examples:
  kthena get templates
  kthena get template deepseek-r1-distill-llama-8b
  kthena get template deepseek-r1-distill-llama-8b -o yaml
  kthena get model-boosters
  kthena get model-boosters --all-namespaces
  kthena get model-servings -n production

### Options

```
  -A, --all-namespaces     List resources across all namespaces
  -h, --help               help for get
  -n, --namespace string   Kubernetes namespace (default: current context namespace)
  -o, --output string      Output format (yaml|json|table)
```

### SEE ALSO

* [kthena](kthena.md)	 - Kthena CLI for managing AI inference workloads
* [kthena get autoscaling-policies](kthena_get_autoscaling-policies.md)	 - List autoscaling policies
* [kthena get autoscaling-policy-bindings](kthena_get_autoscaling-policy-bindings.md)	 - List autoscaling policy bindings
* [kthena get model-boosters](kthena_get_model-boosters.md)	 - List registered models
* [kthena get model-servings](kthena_get_model-servings.md)	 - List model serving workloads
* [kthena get template](kthena_get_template.md)	 - Get a specific template
* [kthena get templates](kthena_get_templates.md)	 - List available manifest templates

