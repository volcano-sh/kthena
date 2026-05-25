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
| networking.kthenaRouter.backendPodHeader.enabled | bool | `false` | Enable X-Kthena-Backend-Pod response header for debug and test traffic. |
| networking.kthenaRouter.debugPort | int | `15000` | Debug server port for Kthena Router (localhost only). |
| networking.kthenaRouter.enabled | bool | `true` | Enable Kthena Router. |
| networking.kthenaRouter.fairness.enabled | bool | `false` | Enable fairness scheduling. |
| networking.kthenaRouter.fairness.inputTokenWeight | float | `1` | Weight multiplier for input tokens. |
| networking.kthenaRouter.fairness.outputTokenWeight | float | `2` | Weight multiplier for output tokens. |
| networking.kthenaRouter.fairness.windowSize | string | `"1h"` | Sliding window duration for token usage tracking. |
| networking.kthenaRouter.gatewayAPI.enabled | bool | `false` | Enable Gateway API related features. |
| networking.kthenaRouter.gatewayAPI.inferenceExtension | bool | `false` | Enable Gateway API Inference Extension features.<br/> Requires `gatewayAPI.enabled` to be true. |
| networking.kthenaRouter.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy for Kthena Router. |
| networking.kthenaRouter.image.repository | string | `"ghcr.io/volcano-sh/kthena-router"` | Image repository for Kthena Router. |
| networking.kthenaRouter.image.tag | string | `"latest"` | Image tag for Kthena Router. |
| networking.kthenaRouter.jwtAuth.enabled | bool | `false` | Allow admission of ModelRoute sessionSticky JWTClaim sources. |
| networking.kthenaRouter.port | int | `8080` | Container port for Kthena Router. |
| networking.kthenaRouter.sessionSticky.redis.address | string | `""` | Redis address for the router session sticky store. Required when `sessionSticky.store` is `redis`. |
| networking.kthenaRouter.sessionSticky.redis.password | string | `""` | Optional Redis password for the router session sticky store. Rendered into `redis-secret` and exposed to the router as `REDIS_PASSWORD`; outside Helm, `REDIS_PASSWORD` can provide it directly. |
| networking.kthenaRouter.sessionSticky.store | string | `"memory"` | Router session sticky store backend. Use `memory` for single-router state or `redis` to share bindings across replicas. |
| networking.kthenaRouter.tls.dnsName | string | `"your-domain.com"` | DNS name to use for the certificate. |
| networking.kthenaRouter.tls.enabled | bool | `false` | Enable TLS for Kthena Router server. |
| networking.kthenaRouter.tls.secretName | string | `"kthena-router-tls"` | Secret name to store the certificate and key. |
| networking.kthenaRouter.webhook.enabled | bool | `true` | Enable webhook for Kthena Router. |
| networking.kthenaRouter.webhook.port | int | `8443` | Container port for Kthena Router webhook. |
| networking.kthenaRouter.webhook.servicePort | int | `443` | Service port for Kthena Router webhook. |
| networking.kthenaRouter.webhook.tls.certFile | string | `"/etc/tls/tls.crt"` | Certificate file path for the webhook. |
| networking.kthenaRouter.webhook.tls.keyFile | string | `"/etc/tls/tls.key"` | Key file path for the webhook. |
| networking.kthenaRouter.webhook.tls.secretName | string | `"kthena-router-webhook-certs"` | Secret name for storing webhook certificates. |
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