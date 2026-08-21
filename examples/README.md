# Examples

This directory contains example manifests for Kthena custom resources and
integrations. Each directory containing a `kustomization.yaml` is an
independently deployable scenario; manifests that are not selected by a
Kustomization remain standalone examples.

## Kustomize scenarios

The initial Kustomize scenario is the GPU-free router quick start. It deploys a
mock vLLM backend, a `ModelServer`, and a `ModelRoute` in the `default`
namespace:

```bash
kubectl apply -k examples/kthena-router
```

Kthena must already be installed in the cluster. See the
[GPU-free quick start](../docs/kthena/docs/getting-started/gpu-free-quick-start.md)
for prerequisites and verification steps.

Render every Kustomize scenario without contacting a cluster by running:

```bash
make kustomize
```

Additional scenarios will be added incrementally. A Kustomization should
represent one coherent deployment and must not aggregate mutually exclusive
examples merely to validate their YAML.
