---
title: Kthena CLI
---
## kthena

Kthena CLI for managing AI inference workloads

### Synopsis

kthena is a CLI tool for managing Kthena AI inference workloads.

For detailed documentation, visit https://kthena.volcano.sh/

It allows you to:
- Create manifests from predefined templates with custom values
- List and view Kthena resources in Kubernetes clusters
- Manage inference workloads, models, and autoscaling policies

Examples:
  kthena get templates
  kthena describe template DeepSeek-R1-Distill-Qwen-32B
  kthena get template DeepSeek-R1-Distill-Qwen-32B -o yaml
  kthena create manifest --name my-model --template DeepSeek-R1-Distill-Qwen-32B
  kthena get model-boosters
  kthena get model-servings --all-namespaces

### Options

```
  -h, --help     help for kthena
  -t, --toggle   Help message for toggle
```

### SEE ALSO

* [kthena create](kthena_create.md)	 - Create kthena resources
* [kthena describe](kthena_describe.md)	 - Show detailed information about a specific resource
* [kthena get](kthena_get.md)	 - Display one or many resources

