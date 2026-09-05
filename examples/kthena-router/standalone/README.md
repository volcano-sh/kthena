# Standalone router resources

Manifests in this directory are read directly by `kthena-router` when it is
started with `--resource-source=file`, so the router can run without a
Kubernetes API server:

```bash
kthena-router \
  --resource-source=file \
  --resource-dir=examples/kthena-router/standalone \
  --resource-sync-period=10s
```

The manifests are exactly the ModelRoute, ModelServer, ExternalModelProvider and
Secret objects that would otherwise be applied to a cluster. Because serving
instances cannot be discovered as pods without an API server, a ModelServer
lists them in `spec.endpoints` instead of `spec.workloadSelector.matchLabels`.

The directory is re-read every `--resource-sync-period`, so adding, changing or
removing a manifest takes effect without restarting the router.
