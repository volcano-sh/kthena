# Introduction

This is the [helm](https://helm.sh/) that Kthena used to deploy on Kubernetes.

Files in `crds/` are custom resource definitions, which are used to define the custom resources used by Kthena.

`Chart.yaml` is a YAML file containing information about the chart.
Visit [here](https://helm.sh/docs/topics/charts/#the-chartyaml-file) for more information.

`charts/` is a directory containing the dependencies of the chart. There are two subcharts `workload` and
`networking` in it.

`values.yaml` is a YAML file containing the default configuration values for this chart.

`templates/` is a directory containing the Kubernetes manifests that will be deployed by this chart.

# Usage

## Prerequisites

- [Helm](https://helm.sh/docs/intro/install/) must be installed.
- [Cert-manager](https://cert-manager.io/docs/installation/) is optional and only required if you enable it by setting `global.certManager.enable` to `true` in `values.yaml`.
- [Webhook certificate](#webhook-certificate-configuration) must be configured.
- **Redis** must be deployed separately if using KV cache or score plugins. See [Redis Deployment](#redis-deployment) section below.


## Install


Syntax:
```shell
helm install <release-name> <chart> [flags]
```
Example:
```shell
helm install my-release my-chart --namespace my-namespace --create-namespace
```

### Installation Customization
You can override the default value of `values.yaml` by using the `--set` flag and `-f`.

```shell
helm install <release-name> <chart> --namespace <namespace> \
  --set workload.enabled=true \
  --set networking.enabled=false \
  -f values-dev.yaml # And you can even specify a customized `values.yaml` file for installation.
```

### Installation Order

Helm first installs resources from the `/crd` directory.  
After that, it installs resources from the `/templates` directory in the following order:
> - Namespace
> - NetworkPolicy
> - ResourceQuota
> - LimitRange
> - PodSecurityPolicy
> - PodDisruptionBudget
> - ServiceAccount
> - Secret
> - SecretList
> - ConfigMap
> - StorageClass
> - PersistentVolume
> - PersistentVolumeClaim
> - CustomResourceDefinition  (CRD)
> - ClusterRole
> - ClusterRoleList
> - ClusterRoleBinding
> - ClusterRoleBindingList
> - Role
> - RoleList
> - RoleBinding
> - RoleBindingList
> - Service
> - DaemonSet
> - Pod
> - ReplicationController
> - ReplicaSet
> - Deployment
> - HorizontalPodAutoscaler
> - StatefulSet
> - Job
> - CronJob
> - Ingress
> - APIService

**NOTICE:**  
HELM manages the installation of CRDs. However, if you need to update or uninstall a CRD, please use `kubectl apply` or `kubectl delete` like below.

```shell
# Update CRDs
kubectl apply -f charts/kthena/charts/networking/crds/
# Note: registry subchart has been removed
kubectl apply -f charts/kthena/charts/workload/crds/ --server-side

# Uninstall CRDs 
kubectl delete -f charts/kthena/charts/networking/crds/
# Note: registry subchart has been removed
kubectl delete -f charts/kthena/charts/workload/crds/
```
> **WARNING:**  
> When you delete a CRD, Kubernetes will automatically delete all Custom Resources (CRs) that were created based on that CRD definition.  
> This can lead to data loss if those CRs hold important configuration or state. Make sure you understand the implications before deleting a CRD.

For more details on the reasoning behind this, see [this explanation](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/#some-caveats-and-explanations) and [these limitations](https://helm.sh/docs/topics/charts/#limitations-on-crds).

## Uninstall

```shell
helm uninstall <release-name> -n <namespace>
```

## Test

### Lint
```shell
helm lint charts/kthena
```
### Validate
```shell
helm template test-release charts/kthena --validate
```
### Debug
```shell
helm template test-release charts/kthena --debug
```

### Dry-run
```shell
helm install test-release charts/kthena --dry-run
```

## Distribution

You have two options for distributing the Helm chart:

### Option 1: Package the Chart as an Archive

Package your chart into a `.tgz` archive file using the following command:

```shell
helm package charts/kthena
```

This creates a versioned archive (e.g., `kthena-0.1.0.tgz`) that can be easily shared or uploaded to a Helm repository.

### Option 2: Generate a Single Manifest File

You can generate a single YAML manifest file (`install.yaml`) from your Helm chart. This file can be applied directly with `kubectl`.

**Without CRDs:**
```shell
helm template my-release charts/kthena > install.yaml
```

**With CRDs included:**
```shell
helm template my-release charts/kthena --include-crds > install.yaml
```

> **Note:** Use the `--include-crds` flag if you want to include Custom Resource Definitions (CRDs) in the generated manifest.


## Webhook Certificate Configuration

The kthena project includes webhooks for validating and mutating resources. These webhooks require TLS certificates to function securely. There are two ways to configure certificates for the webhooks:

### Using cert-manager (Default)

By default, the Helm chart is configured to use cert-manager to automatically provision and manage certificates for the webhooks. This requires cert-manager to be installed in your cluster.

To enable cert-manager integration, set the following in your Helm values:

```yaml
global:
  certManager:
    enabled: true
```

### Manual Certificate Configuration (Fallback)

If cert-manager is not available or you prefer to manage certificates manually, you can provide your own certificates using the CLI parameters when starting the webhooks:

For model-booster-webhook:
```
--tls-cert-file=/path/to/your/cert.crt
--tls-private-key-file=/path/to/your/key.key
```

For model-serving-webhook:
```
--tls-cert-file=/path/to/your/cert.crt
--tls-private-key-file=/path/to/your/key.key
```

When using manual certificate configuration, make sure to disable cert-manager integration in your Helm values and provide the CA bundle:

```yaml
global:
  certManager:
    enabled: false
  webhook:
    caBundle: "base64-encoded-ca-bundle"
```

The CA bundle should be base64-encoded. You can generate it with:
```
cat /path/to/your/ca.crt | base64 | tr -d '\n'
```

You will need to ensure that the certificates are properly mounted into the webhook pods and that the paths match the CLI parameters. The CA bundle is required for the Kubernetes API server to trust the webhook server's certificate.

## Redis Deployment

Redis is required when using Kthena features like **KV cache plugin** or **score plugin**. Redis is **not** included in the Helm chart and must be deployed separately when needed.

### Quick Start

Deploy Redis when using caching features:

```shell
kubectl apply -f examples/redis/redis-standalone.yaml
```

### Configuration

After deploying Redis, Kthena components automatically read Redis connection information from the `redis-config` ConfigMap in the `kthena-system` namespace.

### Custom Redis Deployment

If you have an existing Redis deployment, create the required configuration:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: redis-config
  namespace: kthena-system
data:
  REDIS_HOST: "your-redis-host"
  REDIS_PORT: "6379"
---
apiVersion: v1
kind: Secret
metadata:
  name: redis-secret
  namespace: kthena-system
type: Opaque
data:
  REDIS_PASSWORD: "base64-encoded-password"
```

For detailed information about when Redis is required and deployment instructions, see [examples/redis/README.md](../../examples/redis/README.md).
