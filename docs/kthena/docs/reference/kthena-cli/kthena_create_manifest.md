---
title: Kthena CLI
---
## kthena create manifest

Create resources from a manifest template

### Synopsis

Create kthena resources from predefined manifest templates.

Manifests are predefined combinations of kthena resources that can be
customized with your specific values. This command will:
1. Render the template with your provided values
2. Show you the resulting YAML
3. Ask for confirmation
4. Apply the resources to Kubernetes (unless --dry-run is specified)

Examples:
  kthena create manifest --template basic-inference --name my-model --image my-registry/model:v1.0
  kthena create manifest --template basic-inference --values-file values.yaml
  kthena create manifest --template basic-inference --name my-model --dry-run

```
kthena create manifest [flags]
```

### Options

```
      --dry-run              Show rendered template without applying to cluster
  -h, --help                 help for manifest
      --name string          Name for the inference workload
  -n, --namespace string     Kubernetes namespace to create resources in (default "default")
      --set stringToString   Set template values (can specify multiple or separate values with commas: key1=val1,key2=val2) (default [])
  -t, --template string      Template name to use (required)
  -f, --values-file string   YAML file containing template values
```

### SEE ALSO

* [kthena create](kthena_create.md)	 - Create kthena resources

