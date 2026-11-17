# E2E Tests for Kthena

This directory contains end-to-end (E2E) tests for the Kthena project using Kind (Kubernetes in Docker).

## Overview

The E2E tests use helm to install kthena into a Kind cluster and verify core functionality. Tests are organized into categories that can run in parallel on CI.

## Test Categories

| Category | Description | Make Target |
|----------|-------------|-------------|
| `controller-manager` | ModelServing, ModelBooster, Autoscaling | `make test-e2e-controller-manager` |
| `router` | ModelRoute without Gateway API and router scheduler plugins | `make test-e2e-router` |
| `gateway-api` | ModelRoute with Gateway API | `make test-e2e-gateway-api` |
| `gateway-inference-extension` | HTTPRoute, InferencePool | `make test-e2e-gateway-inference-extension` |

### Router Scheduler Plugin Coverage

The scheduler plugin tests share the Router E2E setup, then apply a focused scheduler configuration and restart the Router for each test.
When multiple Score plugins are enabled, the Router adds each plugin score multiplied by its configured weight. The mixed-weights test makes two plugins prefer different replicas and verifies both weight orderings.

| Test | Signal | Main assertion |
|------|--------|----------------|
| KV-cache aware | Redis block ownership | A cache-hit prompt prefers the owning replica, while misses do not pin to one replica |
| KV-cache aware (memory mode) | Router in-memory index streamed from runtime sidecars, no Redis | A cache-hit prompt prefers the owning replica, while misses do not pin to one replica |
| KV-cache aware (memory mode, pod lifecycle) | Sidecar stream of a replaced pod | A deleted owner is no longer selected, and its replacement can own blocks after the router reconnects |
| KV-cache aware (memory mode, router restart) | Initial snapshot on the sidecar stream | After a router rollout, cache-hit routing resumes without warming the prompt again |
| KV-cache aware (memory mode, NotReady) | Sidecar stream of a pod that turns NotReady | A NotReady pod is not selected, and once Ready again the router reconnects and cache-hit routing resumes without warming again |
| KV-cache aware (memory mode, multiple routers) | One in-memory index per router replica | Every router replica routes a cache-hit prompt to the owning replica |
| Prefix cache | Router prefix history | Repeated prompts prefer the replica with the matching prefix |
| Least request | Waiting requests | Traffic avoids saturated replicas |
| GPU usage | Backend KV-cache utilization | Traffic prefers the replica with lower cache utilization |
| Least latency | Observed TTFT and TPOT | Traffic prefers the faster backend pool |
| LoRA affinity | Loaded adapters | Traffic is filtered to a replica with the requested adapter |
| Random | Random score | Traffic is distributed across ready replicas |
| Mixed weights | KV-cache ownership and GPU usage | Changing the relative plugin weights changes the selected replica |

## Prerequisites

- [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) must be installed
- Go 1.26
- Docker (required by Kind)
- Helm (required to install helm charts)

## Test Environment

The tests create a Kind cluster with the following characteristics:

- **Cluster Name**: `kthena-e2e` (can be overridden with `CLUSTER_NAME` env var)
- **Kubernetes Version**: v1.31.0
- **Test Namespace**: `dev`

## CI Behavior

The GitHub Actions E2E workflow runs the same category targets in parallel by using a matrix:

| CI Job | Make Target | Cluster Name |
|--------|-------------|--------------|
| `E2E (controller-manager)` | `make test-e2e-controller-manager` | `kthena-controller-manager` |
| `E2E (router)` | `make test-e2e-router` | `kthena-router` |
| `E2E (gateway-api)` | `make test-e2e-gateway-api` | `kthena-gateway-api` |
| `E2E (gateway-inference-extension)` | `make test-e2e-gateway-inference-extension` | `kthena-gateway-inference-extension` |

The workflow only runs for pull requests that touch runtime, chart, test, or E2E workflow paths. Documentation-only changes outside those paths should not trigger this workflow.

The controller-manager target runs two Go test processes sequentially in the same
job: `TestModelCR*` covers ModelBooster, and its complement covers the remaining
controller tests. Both groups keep the 20-minute Go timeout and run without result
caching. The real vLLM lifecycle includes a long Pod termination grace period;
separating its test process prevents that time from consuming the other group's
budget. Each process uses the existing `TestMain` installation and namespace
cleanup. The second group still runs if the first fails, and either failure makes
the target fail. All tests and their assertions remain enabled, and the CI job's
50-minute limit is unchanged.

Each category installs only the external CRDs it needs:

| Category | Additional CRDs |
|----------|-----------------|
| `controller-manager` | LeaderWorkerSet |
| `router` | None |
| `gateway-api` | Gateway API |
| `gateway-inference-extension` | Gateway API and Gateway API Inference Extension |
| `all` | Gateway API, Gateway API Inference Extension, and LeaderWorkerSet |

### Router External Provider Mock

The Router category builds `kthena-external-provider-mock` from the repository and loads the image directly into Kind. The image is not pushed to a registry. It serves self-signed HTTPS endpoints for OpenAI Chat Completions, OpenAI Completions, OpenAI Responses, and Anthropic Messages, plus a cluster-local HTTP administration endpoint used only to inspect captured requests.

These tests use fixed fake credentials and do not call real model-provider APIs. They verify protocol and opaque payload passthrough, model rewriting, streaming usage extraction, destination-specific metrics, structured access logs, upstream 429 handling, active-request gauges, and text-only local input-token accounting. In particular, non-text image and file content is forwarded but does not contribute to Router input-token estimates, rate limiting, or related scheduling observations.

`make test-e2e-router` supplies `E2E_EXTERNAL_PROVIDER_MOCK_IMAGE` to the Go E2E package after building and loading that exact image. This is an internal image-reference variable, not a provider credential; users normally do not need to set it.

## Running the Tests

### Run All Tests (Legacy)

```bash
make test-e2e
make test-e2e-cleanup
```

### Run Specific Category

```bash
# Controller manager tests
make test-e2e-controller-manager

# Router tests (no Gateway API, includes scheduler plugins)
make test-e2e-router

# Gateway API tests
make test-e2e-gateway-api

# Gateway Inference Extension tests
make test-e2e-gateway-inference-extension

# Cleanup after any test
make test-e2e-cleanup
```

### Reproduce a CI Category Locally

To reproduce one CI matrix entry locally, use the same cluster name pattern as the workflow:

```bash
CLUSTER_NAME=kthena-gateway-api make test-e2e-gateway-api
CLUSTER_NAME=kthena-gateway-api make test-e2e-cleanup
```

Replace `gateway-api` with the category that failed in CI.

### Environment Variables

- `CLUSTER_NAME`: Override Kind cluster name (default: `kthena-e2e`)
- `TEST_CATEGORY`: Specify which CRDs to install (used by setup.sh)
- `HUB`: Docker image registry (default: `ghcr.io/volcano-sh`)
- `TAG`: Docker image tag (default: `latest`)
- `E2E_EXTERNAL_PROVIDER_MOCK_IMAGE`: Internal image reference supplied by `make test-e2e-router`; it is not an API key or other credential

## Local Testing Considerations

### CPU Limitations (AVX-512)

Some E2E tests (e.g., `TestModelCR` in `model_booster_test.go`) use vLLM-based images (`ghcr.io/huntersman/vllm-cpu-env:latest`).

**Important:** The official vLLM CPU backend relies heavily on the **AVX-512** instruction set.

- **Linux Servers:** Most modern server-grade CPUs (Intel Xeon Scalable, AMD EPYC "Genoa") support AVX-512.
- **Laptops/Intel Macs:** Most consumer-grade Intel CPUs (especially older ones or 12th+ Gen Core where Intel disabled AVX-512) **do not** support it.
- **Apple Silicon:** Architecture mismatch (ARM64 vs x86_64).

If you run these tests on an incompatible machine, the vLLM containers may crash with `SIGILL` (Illegal Instruction). For more details, see the [vLLM CPU installation guide](https://docs.vllm.ai/en/stable/getting_started/installation/cpu/).

### Running Non-vLLM Tests Locally

Tests that do not require the vLLM image (like webhook validation/mutation tests) can be executed locally even without AVX-512 support.

For example, to run the controller manager webhook tests:

```bash
go test -v ./test/e2e/controller-manager/ -run TestWebhook
```

## Troubleshooting

### One CI Category Failed

A failure in one matrix category does not always mean the pull request changed that component. First check whether the PR touched one of the E2E workflow trigger paths, then inspect the failing category logs and uploaded `e2e-logs-*` artifact.

Common first checks:

- Confirm the failing test package from the last `FAIL` line.
- Check whether the test reached cleanup; cleanup logs usually appear after the package failure.
- If only one category failed and the PR does not touch that area, rerun the failed job before changing code.
- For Gateway API failures, confirm the Gateway API CRDs were installed successfully during `setup.sh`.

### Cleanup Left a Cluster Behind

If a local run exits before cleanup, remove the category cluster directly:

```bash
kind delete cluster --name kthena-gateway-api
```
