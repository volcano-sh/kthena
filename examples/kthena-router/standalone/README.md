# Standalone router resources

Manifests in this directory are read directly by `kthena-router` when it is
started with `--config-source=file`, so the router can run without a
Kubernetes API server:

```bash
kthena-router \
  --config-source=file \
  --config-dir=examples/kthena-router/standalone \
  --config-sync-period=10s
```

The manifests are exactly the ModelRoute, ModelServer, ExternalModelProvider and
Secret objects that would otherwise be applied to a cluster. Because serving
instances cannot be discovered as pods without an API server, a ModelServer
lists them in `spec.endpoints` instead of `spec.workloadSelector.matchLabels`.

The directory is re-read every `--config-sync-period`, so adding, changing or
removing a manifest takes effect without restarting the router.
