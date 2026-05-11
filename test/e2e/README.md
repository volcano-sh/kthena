# E2E Tests for Kthena

This directory contains end-to-end (E2E) tests for the Kthena project using Kind (Kubernetes in Docker).

## Overview

The E2E tests use helm to install kthena into a Kind cluster and verify core functionality. Tests are organized into categories that can run in parallel on CI.

## Test Categories

| Category | Description | Make Target |
|----------|-------------|-------------|
| `controller-manager` | ModelServing, ModelBooster, Autoscaling | `make test-e2e-controller-manager` |
| `router` | ModelRoute without Gateway API | `make test-e2e-router` |
| `gateway-api` | ModelRoute with Gateway API | `make test-e2e-gateway-api` |
| `gateway-inference-extension` | HTTPRoute, InferencePool | `make test-e2e-gateway-inference-extension` |

## Prerequisites

- [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) must be installed
- Go 1.24
- Docker (required by Kind)
- Helm (required to install helm charts)

## Test Environment

The tests create a Kind cluster with the following characteristics:

- **Cluster Name**: `kthena-e2e` (can be overridden with `CLUSTER_NAME` env var)
- **Kubernetes Version**: v1.31.0
- **Test Namespace**: `dev`

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

# Router tests (no Gateway API)
make test-e2e-router

# Gateway API tests
make test-e2e-gateway-api

# Gateway Inference Extension tests
make test-e2e-gateway-inference-extension

# Cleanup after any test
make test-e2e-cleanup
```

### Environment Variables

- `CLUSTER_NAME`: Override Kind cluster name (default: `kthena-e2e`)
- `TEST_CATEGORY`: Specify which CRDs to install (used by setup.sh)
- `HUB`: Docker image registry (default: `ghcr.io/volcano-sh`)
- `TAG`: Docker image tag (default: `latest`)

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
