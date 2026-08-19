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

## Tier 2: Combination Tests

Tier 2 combination tests form an orthogonal matrix that isolates traffic conditions (P0), backend heterogeneity (P1), routing strategies (plugin chains), and system architecture from router performance. The matrix is 8 scenarios × 7 plugin chains = 56 sequential runs, using Prometheus-only measurement (no pprof per run; baseline smoke-test-s2 uses `pprof: true`).

### 8 P0/P1 Scenarios

| # | Scenario | File | Goal | Traffic / Backend parameters |
|---|----------|------|------|------------------------------|
| P0.1 | Burstiness | `tier2-p0.1-burstiness.yaml` | Gamma arrival pattern at mid-QPS steady-state | `rate: 50`, `burstiness: 0.3`, homogeneous 4 backends |
| P0.2 | Ramp strategy | `tier2-p0.2-ramp.yaml` | Linear ramp-up to steady QPS | `rate: 50`, `ramp.strategy: linear`, `ramp.duration: 20s`, homogeneous 4 backends |
| P0.4 | Prompt distribution | `tier2-p0.4-prompt-distribution.yaml` | Multi-token mixed prompt lengths | `rate: 50`, prompts: `[{tokens: 128}, {tokens: 512}, {tokens: 2048}]`, homogeneous 4 backends |
| P1.1 | Engine mix | `tier2-p1.1-engine-mix.yaml` | Mixed LLM engine types in same pool | `rate: 50`, profiles: sglang-pool (count: 2) + vllm-pool (count: 2), model: Qwen/Qwen3-0.6B |
| P1.2 | Speedup variance | `tier2-p1.2-speedup-variance.yaml` | Backend compute speed ratio heterogeneity | `rate: 50`, profiles: fast (speedupRatio: 10.0), normal (1.0), slow (0.2) |
| P1.3 | KV Cache variance | `tier2-p1.3-kvcache-variance.yaml` | KV cache block limits across backends | `rate: 50`, profiles: high (kvCacheBlocks: 32768), low (kvCacheBlocks: 4096) |
| P1.5 | maxNumSeqs variance | `tier2-p1.5-maxnumseqs-variance.yaml` | Concurrent request capacity differences | `rate: 50`, profiles: high (maxNumSeqs: 512), low (maxNumSeqs: 64) |
| P1.6 | Latency variance composite | `tier2-p1.6-latency-variance-composite.yaml` | Composite speedup + concurrency variance | `rate: 75`, profiles: fast (speedupRatio: 10.0, maxNumSeqs: 256), normal (1.0, 128), slow (0.2, 32) |

**Note:** All Tier 2 scenarios use `metrics.pprof: false` (Prometheus metrics only) — no pprof sampling per run, significantly reducing test execution time compared to `pprof: true`.

### 7 Plugin Chains (Routing Strategies)

| # | Filename | Chain file | Score plugins enabled | Baseline? | Router-side metric |
|---|----------|------------|-----------------------|-----------|--------------------|
| 1 | `router-config-random.yaml` (existing) | N/A | random (w: 1) | No | Plugin duration histogram for `random/score` |
| 2 | `router-config-least-latency.yaml` (existing) | N/A | least-latency (w: 1, TTFTTPOTWeightFactor: 0.5) | YES | Plugin duration histogram for `least-latency/score` |
| 3 | `router-config-least-request.yaml` (existing) | N/A | least-request (w: 1) | No | Plugin duration histogram for `least-request/score` |
| 4 | `router-config-gpu-usage.yaml` (new) | N/A | gpu-usage (w: 1) | No | Plugin duration histogram for `gpu-usage/score` |
| 5 | `router-config-kvcache-aware.yaml` (new) | N/A | kvcache-aware (w: 1, blockSizeToHash: 16, maxBlocksToMatch: 128) | No | Plugin duration histogram for `kvcache-aware/score` |
| 6 | `router-config-least-latency-gpu-usage.yaml` (new) | N/A | least-latency (w: 1) + gpu-usage (w: 1) | No | Plugin duration histograms for both plugins |
| 7 | `router-config-least-latency-kvcache-aware.yaml` (new) | N/A | least-latency (w: 1) + kvcache-aware (w: 1) | No | Plugin duration histograms for both plugins |

All chains disable `least-request` and `lora-affinity` in `Filter`. Each chain emits its own scheduler plugin duration histogram under `kthena_router_scheduler_plugin_duration_seconds` when the scenario runs.

### Comparison Strategy

The matrix report contains two comparison sub-sections:

1. **`comparisons.vs_least_latency`:** Cross-chain comparisons using chain #2 (`least-latency`) as the production-default baseline. Computes AIPerf end-to-end metric deltas (TTFT, latency, throughput) for all 6 non-baseline chains vs chain #2. Also computes per-plugin router duration deltas where plugin keys intersect: chains #6 and #7 share `least-latency/score` with the baseline, so meaningful plugin-level comparisons exist for those two. Remaining chains (#1, #3, #4, #5) are marked `_skipped: true` because plugin name mismatch yields empty intersection.

2. **`comparisons.cross_scenario`:** Same-chain, cross-scenario comparisons for each of the 7 chains. Compares each scenario's run against a reference scenario (P0.1 burstiness) for that SAME chain. Since the same chain always uses the same Score plugins, plugin key intersection is guaranteed non-empty — yielding meaningful per-plugin latency deltas. Characterizes how traffic/backend conditions affect a given plugin's scheduling overhead (e.g., does `least-latency/score` take longer under bursty traffic?). Includes Prometheus plugin duration histogram bucket data alongside AIPerf end-to-end metrics.

### CLI Usage

```bash
# Run full Tier 2 matrix (all 56 combinations)
python scripts/tier2_matrix.py \
  --scenarios-dir scenarios/ \
  --chains-dir plugins/ \
  --output results/tier2/

# Dry-run mode (validates YAMLs without deploying)
python scripts/tier2_matrix.py \
  --scenarios-dir scenarios/ \
  --chains-dir plugins/ \
  --dry-run

# Visualize the matrix report
python scripts/visualize_matrix.py \
  --input results/tier2/tier2_matrix_report.json \
  --output results/tier2/
```

### Matrix Report Output Format

The report is a structured JSON at `<output_dir>/tier2_matrix_report.json` containing:

```json
{
  "matrix": [
    {
      "scenario": "tier2-p0.1-burstiness",
      "chain": "least-latency",
      "verdict": "<VERDICT_STATUS>",
      "aiperf_metrics": {...},
      "router_analysis": "..."
    }
  ],
  "comparisons": {
    "vs_least_latency": [
      {
        "scenario": "tier2-p0.1-burstiness",
        "baseline_chain": "least-latency",
        "test_chain": "random",
        "end_to_end": {
          "ttft_p50_ms": {"baseline_val": 12.3, "test_val": 15.1, "delta_pct": 22.8, "_regressed": true}
        },
        "router_performance": {
          "comparisons": [...],
          "_skipped": false
        }
      }
    ],
    "cross_scenario": [
      {
        "chain": "least-latency",
        "reference_scenario": "tier2-p0.1-burstiness",
        "test_scenario": "tier2-p0.2-ramp",
        "aiperf_metrics": {...},
        "plugin_durations": [
          {
            "plugin_key": "least-latency/score",
            "histogram_comparison": {...},
            "latency_impact": "..."
          }
        ]
      }
    ]
  }
}
```

### Known Limitations

- **P0.4 prompt weight not plumbed:** The `weight` field in `load.prompts[].weight` is read from YAML but ignored by `_join_token_values` in `load_generator.py` — AIPerf receives comma-separated token means without weighting support.
- **P1.1 only first profile gets CRD:** `_build_model_crds_docs` in `kubernetes.py` reads `profiles[0]` only — only the first profile's engine type gets a ModelServer/ModelRoute CRD deployed, even though multiple profiles are configured.
- **Chains #5/#7 require Redis + tokenizer:** `kvcache-aware` needs a running Redis instance for distributed coordination *and* a tokenizer endpoint on backend pods. In mock-backend environments, kvcache-aware produces nil scores unless Redis + tokenizer are available. The plan recommends integrating Redis deployment into the matrix orchestrator flow if Redis-backed scoring becomes relevant to validation goals — chains are marked `VERDICT_FRAMEWORK_ERROR` if Redis is unavailable.

### Visualization

```bash
# Generate charts from the matrix report
python scripts/visualize_matrix.py \
  --input results/tier2/tier2_matrix_report.json \
  --output results/tier2/charts/
```

Generates four PNG charts visualizing the Tier 2 matrix results:

1. **End-to-end metric heatmap:** 8×7 grid showing each scenario×chain cell color-coded by regression verdict (green=pass, yellow=deltas within tolerance, red=regressed)
2. **Cross-scenario plugin latency bar chart:** For each chain (x-axis shows 5 plugin names), compares plugin scheduling latency across benchmark scenarios relative to the reference scenario (P0.1). Bars colored green/red indicate improvement/worsening.
3. **Scenario parameter impact scatter plot:** Scatter plot of how orthogonal scenario parameters (burstiness, speedupRatio, kvCacheBlocks, maxNumSeqs) affect AIPerf end-to-end latencies across all chains. X-axis represents the normalized scenario parameter value (0→1 scale); Y-axis shows measured TTFT or latency. Each marker labeled with scenario+chain.
4. **Composite metric radar/spider chart:** Radar chart showing multi-dimensional metric comparison (TTFT, latency avg, throughput rps, error rate) for baseline chain vs test chain(s) on P0.4 (most complex scenario). Axes represent different metrics with percentage scaling.

---

## References

- [Kthena Router Benchmark Proposal](../../docs/proposal/kthena-router-benchmark.md)
- [AIPerf Documentation](https://github.com/ai-dynamo/aiperf)
- [Dynamo Mocker Documentation](https://github.com/ai-dynamo/dynamo/blob/main/docs/mocker/mocker.md)
- [Kthena Architecture](../../docs/kthena/docs/architecture/)
