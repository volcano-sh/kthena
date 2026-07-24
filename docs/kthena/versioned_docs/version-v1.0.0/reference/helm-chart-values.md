# kthena

A Helm chart for deploying Kthena

![Version: 1.0.0](https://img.shields.io/badge/Version-1.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 1.0.0](https://img.shields.io/badge/AppVersion-1.0.0-informational?style=flat-square)

## Requirements

| Repository | Name | Version |
|------------|------|---------|
|  | networking | 1.0.0 |
|  | workload | 1.0.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| global.certManagementMode | string | `"auto"` | Certificate Management Mode.<br/>  Three mutually exclusive options for managing TLS certificates:<br/>  - `auto`: Webhook servers generate self-signed certificates automatically.<br/>  - `cert-manager`: Use cert-manager to generate and manage certificates (requires cert-manager installation).<br/>  - `manual`: Provide your own certificates via caBundle. |
| global.webhook.caBundle | string | `""` | CA bundle for webhook server certificates (base64-encoded).<br/> This is ONLY required when `certManagementMode` is set to "manual".<br/> You can generate it with: `cat /path/to/your/ca.crt | base64 | tr -d '\n'`<br/> |
| networking.enabled | bool | `true` | Enable the networking subchart. |
| networking.kthenaRouter.debugPort | int | `15000` | Debug server port for Kthena Router (localhost only). |
| networking.kthenaRouter.drainTimeout | string | `"5m"` | This should be less than terminationGracePeriodSeconds. |
| networking.kthenaRouter.enabled | bool | `true` | Enable Kthena Router. |
| networking.kthenaRouter.fairness.enabled | bool | `false` | Enable user-fairness scheduling. Mutually exclusive with sessionBoost. |
| networking.kthenaRouter.fairness.inputTokenWeight | float | `1` | User-fairness strategy: weight multiplier for input tokens. |
| networking.kthenaRouter.fairness.maxConcurrent | int | `0` | Global total inflight request limit admitted through the fairness gate.<br/> `0` or unset falls back to QPS-based rate limiting. |
| networking.kthenaRouter.fairness.outputTokenWeight | float | `2` | User-fairness strategy: weight multiplier for output tokens. |
| networking.kthenaRouter.fairness.windowSize | string | `"1h"` | User-fairness strategy: sliding window duration for token usage tracking. |
| networking.kthenaRouter.gatewayAPI.enabled | bool | `false` | Enable Gateway API related features. |
| networking.kthenaRouter.gatewayAPI.inferenceExtension | bool | `false` | Enable Gateway API Inference Extension features.<br/> Requires `gatewayAPI.enabled` to be true. |
| networking.kthenaRouter.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy for Kthena Router. |
| networking.kthenaRouter.image.repository | string | `"ghcr.io/volcano-sh/kthena-router"` | Image repository for Kthena Router. |
| networking.kthenaRouter.image.tag | string | `"latest"` | Image tag for Kthena Router. |
| networking.kthenaRouter.port | int | `8080` | Container port for Kthena Router. |
| networking.kthenaRouter.sessionBoost.enabled | bool | `false` | Enable session-boost scheduling. Mutually exclusive with fairness. |
| networking.kthenaRouter.sessionBoost.gracePeriod | string | `"0s"` | Wait time after a request completes for a same-session follow-up.<br/> Disabled by default (`0s`). |
| networking.kthenaRouter.sessionBoost.header | string | `"X-Session-ID"` | HTTP header used to identify conversation sessions. |
| networking.kthenaRouter.sessionBoost.inflightPerPod | int | `16` | Maximum inflight requests admitted per backend pod.<br/> Total inflight limit is this value times the number of backend pods.<br/> `0` or unset uses the router default (16). |
| networking.kthenaRouter.sessionBoost.maxSessions | int | `4096` | Maximum number of recently-completed sessions kept warm for boosting.<br/> Bounds an LRU cache; the least-recently-used session is evicted automatically.<br/> Size it by the number of concurrent conversations to keep boosted. |
| networking.kthenaRouter.sessionBoost.timeout | string | `"30s"` | Maximum time a request may wait in the session-boost queue before it is<br/> rejected with HTTP 504. Defaults to `30s`; set a non-positive duration<br/> (e.g. `0s`) to disable it (bounded only by client disconnect). |
| networking.kthenaRouter.terminationGracePeriodSeconds | int | `330` | The router will drain all in-flight requests before forcefully closing connections. |
| networking.kthenaRouter.tls.dnsName | string | `"your-domain.com"` | DNS name to use for the certificate. |
| networking.kthenaRouter.tls.enabled | bool | `false` | Enable TLS for Kthena Router server. |
| networking.kthenaRouter.tls.secretName | string | `"kthena-router-tls"` | Secret name to store the certificate and key. |
| networking.kthenaRouter.webhook.enabled | bool | `true` | Enable webhook for Kthena Router. |
| networking.kthenaRouter.webhook.port | int | `8443` | Container port for Kthena Router webhook. |
| networking.kthenaRouter.webhook.servicePort | int | `443` | Service port for Kthena Router webhook. |
| networking.kthenaRouter.webhook.tls.certFile | string | `"/etc/tls/tls.crt"` | Certificate file path for the webhook. |
| networking.kthenaRouter.webhook.tls.keyFile | string | `"/etc/tls/tls.key"` | Key file path for the webhook. |
| networking.kthenaRouter.webhook.tls.secretName | string | `"kthena-router-webhook-certs"` | Secret name for storing webhook certificates. |
| workload.controllerManager.autoscalingSyncPeriodSeconds | int | `0` | Reconcile interval in seconds for the autoscaler. Smaller values react faster to traffic spikes but increase API server load. 0 uses the binary default (15). |
| workload.controllerManager.debugPort | int | `0` | Debug server port for Controller Manager (set 0 to disable). |
| workload.controllerManager.downloaderImage.repository | string | `"ghcr.io/volcano-sh/downloader"` | Image repository for the Downloader. |
| workload.controllerManager.downloaderImage.tag | string | `"latest"` | Image tag for the Downloader. |
| workload.controllerManager.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy for the Controller Manager. |
| workload.controllerManager.image.repository | string | `"ghcr.io/volcano-sh/kthena-controller-manager"` | Image repository for the Controller Manager. |
| workload.controllerManager.image.tag | string | `"latest"` | Image tag for the Controller Manager. |
| workload.controllerManager.runtimeImage.repository | string | `"ghcr.io/volcano-sh/runtime"` | Image repository for the Runtime. |
| workload.controllerManager.runtimeImage.tag | string | `"latest"` | Image tag for the Runtime. |
| workload.controllerManager.webhook.enabled | bool | `true` | Enable webhook for the Controller Manager. |
| workload.controllerManager.webhook.tls.certSecretName | string | `"kthena-controller-manager-webhook-certs"` | Secret name for storing webhook certificates. |
| workload.controllerManager.webhook.tls.serviceName | string | `"kthena-controller-manager-webhook"` | Service name for the webhook. |
| workload.enabled | bool | `true` | Enable the workload subchart. |

## Notes

- Values marked as “usually set by CI” are automatically updated during the release process; manual changes are not required.
- For detailed information about each component, refer to the corresponding architecture and user guide documents.
- Always review the [values.yaml](https://github.com/volcano-sh/kthena/blob/main/charts/kthena/values.yaml) file in the repository for the latest defaults and available options.