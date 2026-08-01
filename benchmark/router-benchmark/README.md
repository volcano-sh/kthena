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

## CI Runner Capacity and Validated Scenario Parameters

**Scenario load parameters must be calibrated against the actual CI runner's capacity, not guessed.** Issue [#1452](https://github.com/volcano-sh/kthena/issues/1452) tracked exactly this failure mode: an early version of this benchmark picked `rate`/`concurrency` values that saturated the mock backend before the router or plugin under test was meaningfully stressed, so every plugin comparison was actually just measuring how the backend collapsed under too much load — success rates as low as 13-33%, both arms failing near-identically regardless of which scheduler plugin was active.

The GitHub-hosted CI runner these workflows run on has **4 CPU / 16GB RAM**, hosting the entire stack on one node: the Kind control plane, `kthena-controller-manager`, `kthena-router`, and every mocker pod. This is a small, fixed budget — a scenario's `rate`/`concurrency`, mocker pod `count`, and per-pod resource `requests`/`limits` all have to fit inside it simultaneously, and there is no way to derive a safe combination analytically; it has to be measured.

The following operating points have been empirically validated by running the actual scenario against this actual CI runner repeatedly (not derived from local testing, and not accepted on a single run — see the per-scenario comments in the YAML files for the full experiment history and CI run IDs):

| Scenario | Validated load | Mocker pods | Per-pod resources | Result |
|---|---|---|---|---|
| `smoke-test-s2` | `rate: 3`, `duration: 45s` | 8 | `requests: {cpu: 250m, memory: 256Mi}`, `limits: {cpu: 1, memory: 1Gi}` | 100%/100% success (random/least-request), 0 mocker restarts, 0 genuine AIPerf errors, across 2 independent runs |
| `smoke-test-s3` | `concurrency: 5` | 8 | same as above | >97% success both plugins, 0 mocker restarts, ~2.4s p95, across 2 independent runs |

Loads that were tried and rejected, to save the next person from re-discovering the same dead ends: `s2` at `rate: 20` (known-unsafe baseline, ~31-33% success) and `rate: 5` (unstable across three repeated runs, 74-99% depending on the run and whether the 45s or 60s duration was used — increasing the duration made it *worse*, not better, ruling out "the fixed window's cutoff is the whole problem" as an explanation). See `smoke-test-s2.yaml`'s and `smoke-test-s3.yaml`'s header comments for the full numbers and CI run IDs.

`s1`, `s4`-`s8` have not yet been through this calibration process and their current `rate`/`concurrency` values should not be assumed safe — several of them (`s1`, `s7`, `s8` in particular) combine a higher offered rate with fewer pods and no per-pod resource shrink than either validated scenario above, which is exactly the combination that caused the original saturation.

## References

- [Kthena Router Benchmark Proposal](../../docs/proposal/kthena-router-benchmark.md)
- [AIPerf Documentation](https://github.com/ai-dynamo/aiperf)
- [Dynamo Mocker Documentation](https://github.com/ai-dynamo/dynamo/blob/main/docs/mocker/mocker.md)
- [Kthena Architecture](../../docs/kthena/docs/architecture/)
