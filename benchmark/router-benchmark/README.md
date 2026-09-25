# Kthena Router A/B Test Framework

A performance benchmarking framework for the Kthena Router based on the "sandwich model", using AIPerf as the load generator and Dynamo Mocker as the mock backend.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Sandwich Isolation Model                         │
│                                                                         │
│  ┌──────────────┐      ┌──────────────────┐      ┌──────────────────┐   │
│  │   AIPerf     │      │  Kthena Router   │      │  Dynamo Mocker   │   │
│  │  (Load Gen)  │─────►│   (Under Test)   │─────►│  (Mock Backend)  │   │
│  │              │      │                  │      │                  │   │
│  │ • QPS control│      │ • Routing        │      │ • TTFT simulation│   │
│  │ • Concurrency│      │ • Conn. pooling  │      │ • TPOT simulation│   │
│  │ • Arrival    │      │ • Load balancing │      │ • KV Cache sim.  │   │
│  │   distribution     │ • Failover       │      │ • Prom. metrics  │   │
│  └──────────────┘      └──────────────────┘      └──────────────────┘   │
│         │                      │                         │              │
│         └──────────────────────┼─────────────────────────┘              │
│                                │                                        │
│                         Metrics Collector                               │
│               (AIPerf Output + Router Prometheus + Router pprof)        │
└─────────────────────────────────────────────────────────────────────────┘
```

### Why AIPerf + Dynamo Mocker

| Component | Rationale |
|-----------|-----------|
| AIPerf | Official NVIDIA tool; supports multiple arrival patterns (Poisson/Gamma/Constant), credit-based flow control, a real-time TUI dashboard, and detailed TTFT/TPOT metrics |
| Dynamo Mocker | GPU-free, high-fidelity LLM inference simulation; supports vLLM/SGLang engine modes, KV cache simulation, prefix caching, and a configurable latency model |
| K8s deployment | Reuses kthena's existing Helm charts and CRD definitions, consistent with the E2E test infrastructure |

## Current Module Structure

To make `ab_test` easier to review and to gradually align with the layered design in the proposal, the script is split into the following modules:

```
router-benchmark/
├── scripts/
│   ├── ab_test.py                         # CLI entrypoint
│   └── router_ab_test/
│       ├── __init__.py
│       ├── models.py                      # ScenarioConfig / BenchmarkResult
│       ├── kubernetes.py                  # K8sManager: apply, rollout, probe, port-forward
│       ├── load_generator.py              # AIPerfRunner: scenario -> aiperf CLI
│       ├── metrics_collector.py           # MetricsCollector: Prometheus / pprof collection
│       ├── orchestrator.py                # ABTestOrchestrator: drives the A/B flow
│       └── reporter.py                    # ResultReporter: compare / write / print report
└── tests/
    └── test_ab_test.py
```

### Module Responsibilities

- `scripts/ab_test.py`
  - Keeps the original entrypoint path so existing commands, docs, and tests stay valid
  - Provides the CLI parser and `main()`
- `scripts/router_ab_test/models.py`
  - Benchmark domain models and scenario config loading
  - `ScenarioConfig.metrics` defines whether Prometheus / pprof collection is enabled per scenario
  - `BenchmarkResult.artifacts` holds additional collection results
- `scripts/router_ab_test/kubernetes.py`
  - Kubernetes resource operations, router rollout, service/debug port-forwarding, route ready probe
- `scripts/router_ab_test/load_generator.py`
  - Maps the scenario YAML to AIPerf CLI arguments
- `scripts/router_ab_test/metrics_collector.py`
  - Scrapes the router `/metrics` endpoint
  - Fetches the router `/debug/pprof/profile` and other profiles
  - Writes metrics and profile files to `artifacts/<config>/`
- `scripts/router_ab_test/orchestrator.py`
  - Drives the end-to-end A/B execution flow
  - Triggers the Metrics Collector after each AIPerf run
- `scripts/router_ab_test/reporter.py`
  - Compares metrics, builds the report structure, writes JSON, prints the summary

## Quick Start

### Prerequisites

- Docker Desktop or Podman
- Kind (Kubernetes in Docker)
- Helm 3.x
- kubectl
- Python 3.10+

### 1. Create a Kind cluster and deploy Kthena

```bash
cd /path/to/kthena
./hack/local-up-kthena.sh
```

### 2. Install AIPerf

```bash
pip install 'aiperf>=0.9,<0.11'
```

### 3. Run an A/B test

```bash
python scripts/ab_test.py \
  --scenario scenarios/smoke-test-s2.yaml \
  --router-config-a plugins/router-config-random.yaml \
  --router-config-b plugins/router-config-least-latency.yaml \
  --output /tmp/results/
```

#### Router Endpoint Access Modes

Two modes are supported for reaching the router endpoint, selected via `--endpoint-mode`:

| Mode | Scenario | Description |
|------|----------|-------------|
| `pf` (default) | Kind test clusters | Uses `kubectl port-forward` to forward the router Service port to `localhost:<local_port>` |
| `lb` | Production clusters | Reads the EXTERNAL-IP from the LoadBalancer and forms `<external_ip>:<service_port>` |

> Note: the debug port (15000) is not exposed via a Service; both modes use port-forward for it.

## Scenario Configuration

A scenario config follows the sandwich model and has three parts — `load`, `backends`, and `metrics` — as shown in `smoke-test-s2.yaml`.

The `load.schedule.mode` field selects exactly one load-control model:

| Mode | Load model | Emitted AIPerf flag |
|------|-----------|---------------------|
| `rate` / `constant_rate` | Open-loop, rate-based | `--request-rate` (plus arrival-pattern/ramp from `traffic`) |
| `concurrency` | Closed-loop, concurrency-based | `--concurrency` (plus concurrency ramp) |

Only one mode is emitted per run — passing both `--request-rate` and `--concurrency` would make the effective load ambiguous.

## A/B Test Flow

```
1. Apply router config A
2. Restart and wait for router rollout
3. Wait for backend deployment ready
4. Start kubectl port-forward to router service
5. Start kubectl port-forward to router debug port
6. Probe /v1/chat/completions until the route is really warm
7. Run AIPerf and collect result A
8. Scrape router /metrics and fetch pprof artifacts for config A
9. Cold-start backend Pods, Apply router config B
10. Repeat warmup + benchmark + metrics collection for config B
11. Compare metrics and write report_<scenario>.json
12. Exit non-zero if report contains regression
```

## Output

Results are written to the directory given by `--output`. Current outputs:

- `runs/config_a/` and `runs/config_b/`
  - Raw AIPerf output directories
- `artifacts/config_a/` and `artifacts/config_b/`
  - Router Prometheus and pprof collection results
- `report_<scenario>.json`
  - A/B comparison result
  - Includes scheduler/plugin top functions from each collected pprof profile

Pprof analysis is produced automatically by `ResultReporter.build_report()` when a run contains pprof artifacts. The JSON report records flat and cumulative values for the ten hottest `kthena-router/scheduler` functions in each profile.

The parser tests use captured CPU and heap profiles from the router pprof endpoint under `tests/testdata/pprof` so they exercise the real protobuf wire format.

To regenerate the checked-in protobuf binding, run the following from `benchmark/router-benchmark`:

```bash
python -m grpc_tools.protoc -I scripts/router_ab_test --python_out=scripts/router_ab_test scripts/router_ab_test/profile.proto
```

Then run `make gen-copyright` from the repository root to restore the generated file's license header.

## Test Scenarios

Eight scenarios are designed:

| # | Scenario | Goal | Key parameters |
|---|----------|------|----------------|
| S1 | Throughput Baseline | Maximum sustainable throughput | Gradually increasing QPS |
| S2 | Latency vs QPS | Routing overhead under different loads | QPS: 10, 50, 100, 200, 500 |
| S3 | Concurrency Scaling | Connection pool behavior | Connections: 10, 100, 500, 1000 |
| S4 | Backend Count Impact | Scheduler scaling with pod count | Backends: 1, 4, 16, 32 |
| S5 | Prompt Length Impact | Request-body parsing overhead | Prompt tokens: 100, 1000, 4000 |
| S6 | Long Response | SSE relay overhead | Response tokens: 100, 1000, 4096 |
| S7 | Backend Latency Variance | Scheduling with heterogeneous backends | 3 pods: TTFT 10/100/500ms |
| S8 | Routing Strategy Comparison | Routing strategy overhead | random vs least-latency vs least-request |

## References

- [Kthena Router Benchmark Proposal](../../docs/proposal/kthena-router-benchmark.md)
- [AIPerf Documentation](https://github.com/ai-dynamo/aiperf)
- [Dynamo Mocker Documentation](https://github.com/ai-dynamo/dynamo/blob/main/docs/mocker/mocker.md)
- [Kthena Architecture](../../docs/kthena/docs/architecture/)
