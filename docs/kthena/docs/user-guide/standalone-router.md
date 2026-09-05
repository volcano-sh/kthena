# Standalone Router (File-Based Resources)

## Overview

By default Kthena Router watches ModelRoute, ModelServer and
ExternalModelProvider objects on the Kubernetes API server. With
`--resource-source=file` the router instead reads the very same manifests from a
local directory, which lets it run without a cluster, for example for local
development, benchmarking or a container-only deployment.

The APIs are unchanged: the files hold exactly the objects you would otherwise
apply with `kubectl`, so a configuration can be moved between both modes.

## Running the router with file-based resources

```bash
kthena-router \
  --resource-source=file \
  --resource-dir=/etc/kthena/resources \
  --resource-sync-period=10s
```

| Flag                     | Default                 | Description                                                    |
| ------------------------ | ----------------------- | -------------------------------------------------------------- |
| `--resource-source`      | `kubernetes`            | Where resources are read from. One of `kubernetes` or `file`.  |
| `--resource-dir`         | `/etc/kthena/resources` | Directory holding the manifests when `--resource-source=file`. |
| `--resource-sync-period` | `10s`                   | How often the directory is re-read.                            |

The router reads every `.yaml`, `.yml` and `.json` file directly inside the
directory; sub-directories are ignored. A file may contain several documents
separated by `---`. Resources without `metadata.namespace` are placed in the
`default` namespace.

Supported kinds are `ModelRoute`, `ModelServer`, `ExternalModelProvider` and
`Secret`. Any other kind is ignored with a warning.

Because there is no API server in this mode, the admission webhook is disabled
automatically and `--enable-gateway-api` cannot be used. The manifests are
instead validated with the same rules when the directory is loaded; if any
document is invalid, the whole snapshot is rejected and the router keeps
serving the last good one.

## Configuring serving instances

Serving instances cannot be discovered as pods without an API server, so a
ModelServer lists them in `spec.endpoints`:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: deepseek-r1-1-5b
  namespace: default
spec:
  endpoints:
  - name: vllm-0
    address: 127.0.0.1
    port: 8000
  - name: vllm-1
    address: 127.0.0.1
    port: 8001
  model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B"
  inferenceEngine: "vLLM"
```

| Field     | Description                                                                                                    |
| --------- | -------------------------------------------------------------------------------------------------------------- |
| `name`    | Identifies the instance in scheduling, metrics and the debug endpoints as `<ModelServer name>:<name>`. Must be unique within the ModelServer. |
| `address` | IP address or DNS name of the instance.                                                                        |
| `port`    | Port of the instance. Defaults to `spec.workloadPort.port` and is required when that port is unset.            |
| `labels`  | Labels matched by `spec.workloadSelector.pdGroup` to assign a prefill or decode role.                          |

`spec.endpoints` and `spec.workloadSelector.matchLabels` are mutually exclusive.
Static endpoints are ordinary serving instances for the rest of the router: they
are scheduled, scraped for engine metrics and proxied to exactly like discovered
pods. The field also works on a cluster, which is useful for routing to model
servers that run outside of it.

### Prefill/decode disaggregation

To disaggregate statically configured instances, keep `spec.workloadSelector` for
the `pdGroup` definition only and label each endpoint:

```yaml
spec:
  workloadSelector:
    pdGroup:
      groupKey: pd-group
      prefillLabels:
        role: prefill
      decodeLabels:
        role: decode
  endpoints:
  - name: prefill-0
    address: 127.0.0.1
    port: 8000
    labels:
      role: prefill
      pd-group: group-a
  - name: decode-0
    address: 127.0.0.1
    port: 8001
    labels:
      role: decode
      pd-group: group-a
```

## Updating resources

The directory is re-read every `--resource-sync-period`. Added, changed and
removed manifests are applied to the running router, so no restart is needed. A
manifest that fails to parse is reported in the router log and the last valid
snapshot keeps serving traffic.

A complete example is available in
[examples/kthena-router/standalone](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router/standalone).
