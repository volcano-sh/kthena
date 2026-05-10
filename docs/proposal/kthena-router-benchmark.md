# Proposal: Kthena Router Benchmark Framework based on Mocked LLM Backend

- **Status**: Draft
- **Issue**: [#942](https://github.com/volcano-sh/kthena/issues/942)
- **Author**: [Leo Xie]

---

## Summary

This proposal introduces a reusable, config-driven benchmark framework for the Kthena Router — the LLM routing data-plane component of the Volcano/Kthena project. The framework enables reproducible performance measurement under realistic LLM traffic patterns, produces structured reports for regression detection across releases, and supports local and CI execution. Where the benchmark surfaces clear bottlenecks, targeted optimizations will be proposed and submitted as upstream PRs with before/after performance data.

## Motivation

Kthena Router is the critical path for every inference request in a Kthena deployment. As the project matures toward production-grade reliability, the community lacks:

1. **Reproducible performance baselines** — No standardized way to measure throughput, latency, or resource utilization under controlled conditions
2. **Regression detection** — No automated mechanism to catch performance regressions introduced by PRs or dependency updates
3. **Bottleneck visibility** — No tooling to identify hotspots in the request processing pipeline (routing overhead, connection pooling, serialization costs)
4. **Capacity planning data** — No benchmarks to help operators size Router deployments for their expected traffic

The existing [`benchmark/kthena-router/`](https://github.com/volcano-sh/kthena/tree/main/benchmark/kthena-router) tool is a thin wrapper around `sglang/bench_serving.py` packaged as a Kubernetes Job. It measures end-to-end throughput but lacks scenario configurability, CI integration, router-specific metrics, and bottleneck analysis — making it unsuitable for the community's growing needs.

### Goals

- Design and implement a **reusable benchmark framework** (load generator, scenario configuration, metrics collection, result aggregation) that runs both locally and in CI
- Define a **standardized benchmark scenario format** (YAML) covering typical LLM routing patterns:
  - Variable QPS and concurrency levels
  - Short vs. long prompt/response lengths
  - Variable number of mock backends
  - Different routing strategies (ModelRoute, HTTPRoute, ExternalModelProvider)
- Build a **mock LLM inference backend** in Go that simulates realistic streaming token generation with configurable latency profiles (TTFT, TPOT, token burst characteristics)
- Produce a **comprehensive benchmark report** with throughput, TTFT, TPOT, latency percentiles (P50/P95/P99), CPU/memory/goroutine metrics, and bottleneck analysis via `pprof`
- Document the **end-to-end test procedure as a runbook** covering cluster preparation, mock/real backend deployment, benchmark execution, and result interpretation
- **Identify and optimize** up to 3 clear performance hotspots, submitting upstream PRs with before/after comparison data

### Non-Goals

- **Real LLM inference backends** — The framework uses mock backends by default. Testing against real GPUs and inference engines (vLLM, SGLang) is out of scope for this LFX term, as GPU availability cannot be guaranteed and would introduce environment-specific variance.
- **Production SLO monitoring** — The framework targets offline benchmarking, not continuous production monitoring. Integration with Prometheus/Grafana dashboards for ongoing SLO tracking is future work.
- **Comparative benchmarks against other routers** — We benchmark Kthena Router against its own historical baselines, not against other projects (e.g., Envoy AI Gateway, LiteLLM proxy).
- **gRPC routing benchmarks** — Initial scope covers HTTP/SSE routing only. gRPC streaming benchmarks are deferred to a future iteration.
- **Optimization guarantee** — The optimization deliverable is conditional on the benchmark surfacing identifiable bottlenecks. If the Router demonstrates excellent performance with no clear hotspots, the optimization PR is replaced with a performance characterization report and recommendations for future optimization targets.

## Proposal

### Framework Architecture

The benchmark framework consists of four loosely coupled components, each independently configurable and testable:

```
┌──────────────────────────────────────────────────────────────────┐
│                    Benchmark Orchestrator (CLI)                   │
│  - Reads scenario config                                          │
│  - Coordinates lifecycle of all components                        │
│  - Aggregates results and generates report                        │
└──────┬──────────────┬────────────────┬───────────────────────────┘
       │              │                │
       ▼              ▼                ▼
┌─────────────┐ ┌─────────────┐ ┌──────────────────┐
│   Load      │ │   Metrics   │ │   Result         │
│  Generator  │ │  Collector  │ │   Reporter       │
│             │ │             │ │                  │
│ - HTTP/SSE  │ │ - Router    │ │ - JSON output    │
│ - Concurrency│ │   Prometheus│ │ - Markdown report│
│ - Rate limit │ │   metrics   │ │ - CSV export     │
│ - Token-aware│ │ - pprof CPU │ │ - Comparison vs  │
│   metrics    │ │   /mem snap │ │   baseline       │
└──────┬───────┘ └──────┬──────┘ └──────────────────┘
       │                │
       ▼                ▼
┌──────────────────────────────────────────────┐
│            Target Environment                 │
│  ┌──────────────┐    ┌──────────────────────┐│
│  │ Kthena Router│◄───│ Mock LLM Backend(s)  ││
│  │  (under test)│    │ - Streaming response  ││
│  └──────────────┘    │ - Configurable TTFT   ││
│                      │ - Configurable TPOT   ││
│                      │ - Variable token count││
│                      └──────────────────────┘│
└──────────────────────────────────────────────┘
```

#### 1. Benchmark Orchestrator (`cmd/benchmark/main.go`)

A Go CLI tool that:
- Parses a YAML scenario configuration file
- Deploys Router + mock backends into the target Kubernetes cluster (kind for local, ephemeral cluster for CI)
- Waits for all components to be healthy
- Starts the Load Generator
- Starts the Metrics Collector
- Runs the benchmark for the configured duration
- Collects results, tears down resources, generates the report
- Exits with code 0 on success, non-zero on benchmark failure or resource exhaustion

#### 2. Load Generator (`pkg/benchmark/loadgen/`)

A Go-based HTTP load generator tailored for LLM serving benchmarks. Key features:

- **Concurrency model**: Configurable number of concurrent virtual users (VUs), each maintaining a persistent HTTP connection for SSE streaming
- **Rate control**: Supports both constant rate (requests/second) and ramp-up profiles (e.g., 10→50→100 QPS over phases)
- **Token-aware metrics**: Tracks TTFT (time to first response byte) and TPOT (inter-chunk interval) from SSE stream timing
- **Configurable request body**: Supports variable prompt lengths (short ~100 tokens → long ~4000 tokens) using tokenizer-aware payload generation
- **Output**: Latency histograms, success/error counts, throughput (requests/sec and tokens/sec), streamed to Metrics Collector

#### 3. Metrics Collector (`pkg/benchmark/metrics/`)

Collects data from multiple sources during a benchmark run:

- **Load Generator metrics**: Latency histograms, throughput, error rates (via in-process shared memory or gRPC streaming)
- **Router Prometheus metrics**: Scrapes Router's `/metrics` endpoint for Go runtime metrics (goroutines, heap, GC), HTTP handler latencies, connection pool stats
- **pprof profiles**: Takes CPU profile snapshots and heap profiles during steady-state benchmark phase
- **System metrics** (optional): If running alongside node-exporter, collects CPU/memory usage of the Router pod

Data is stored in memory during the run and serialized to JSON at completion.

#### 4. Result Reporter (`pkg/benchmark/reporter/`)

Generates outputs from raw benchmark data:

- **JSON**: Machine-readable result file for automated comparison and CI assertions
- **Markdown report**: Human-readable summary with tables and key findings, suitable for pasting into GitHub issues
- **Comparison mode**: When a baseline result file is provided, computes deltas and highlights regressions (>5% latency increase, >10% throughput decrease)
- **CSV export**: For further analysis in spreadsheet tools

### Benchmark Scenario Configuration

Benchmarks are defined via YAML configuration files for reproducibility:

```yaml
# benchmark.yaml
name: "router-throughput-50qps"
description: "Baseline throughput test at 50 QPS with 4 backends"

environment:
  router:
    image: "kthena-router:latest"
    replicas: 1
    resources:
      cpu: "2"
      memory: "4Gi"

backends:
  count: 4
  mock:
    image: "kthena-mock-backend:latest"
    ttftMean: "50ms"
    ttftStddev: "10ms"
    tpotMean: "15ms"
    tpotStddev: "5ms"
    responseTokens: 500

load:
  phases:
    - duration: "30s"
      rate: 10          # ramp-up: 10 req/s
    - duration: "60s"
      rate: 50          # steady-state: 50 req/s
    - duration: "30s"
      rate: 10          # cool-down

  concurrency: 100      # max concurrent connections
  prompts:
    - name: "short"
      tokens: 100
      weight: 7         # 70% short prompts
    - name: "long"
      tokens: 2000
      weight: 3         # 30% long prompts

routing:
  strategy: "round-robin"  # round-robin | least-connection | random

metrics:
  pprof: true           # capture CPU/memory profiles during steady-state
  prometheus: true      # scrape Router metrics endpoint

output:
  dir: "./results/"
  formats: ["json", "markdown"]
```

### Mock LLM Inference Backend (`pkg/benchmark/mockbackend/`)

A purpose-built Go HTTP server that simulates an OpenAI-compatible `/v1/chat/completions` endpoint with SSE streaming. This is essential because:

- Real LLM backends require GPUs, which are expensive and not available in CI
- Mock backends provide **deterministic, reproducible latency profiles**, which real backends cannot guarantee due to hardware variance
- Configurable latency parameters allow systematic exploration of performance under different backend conditions

**Behavior:**

1. Receive `POST /v1/chat/completions` with an OpenAI-compatible request body
2. Parse `max_tokens` (or use configured default) to determine how many tokens to generate
3. After a configurable TTFT delay (Gaussian distribution), send the first SSE chunk: `data: {"choices":[{"delta":{"content":"tok"},"index":0}]}`
4. Continue sending tokens with configurable TPOT interval between each chunk
5. Send `data: [DONE]` to signal stream completion
6. Close the SSE connection

**Configuration** (via environment variables or flags):
| Parameter | Default | Description |
|-----------|---------|-------------|
| `TTFT_MEAN` | 50ms | Mean time-to-first-token |
| `TTFT_STDDEV` | 10ms | Standard deviation of TTFT |
| `TPOT_MEAN` | 15ms | Mean time-per-output-token |
| `TPOT_STDDEV` | 5ms | Standard deviation of TPOT |
| `DEFAULT_TOKENS` | 500 | Default token count when `max_tokens` not in request |
| `ERROR_RATE` | 0.0 | Simulated error rate (0.0–1.0) for chaos testing |

### Test Scenarios

The following scenarios will be delivered as pre-built YAML configuration files:

| Scenario | Purpose | Key Parameters |
|----------|---------|---------------|
| **Throughput Baseline** | Establish max throughput | Ramp QPS until 50% error rate |
| **Latency vs. QPS** | Measure latency at different loads | QPS: 10, 50, 100, 200, 500 |
| **Concurrency Scaling** | Router scaling under connections | Concurrency: 10, 100, 500, 1000 |
| **Backend Count Impact** | Load balancing overhead | Backends: 1, 4, 16, 32 |
| **Prompt Length Impact** | Cost of large request bodies | Prompt tokens: 100, 1000, 4000 |
| **Long Response** | Streaming overhead for long outputs | Response tokens: 100, 1000, 4096 |
| **Backend Latency Variance** | Behavior under slow/uneven backends | TTFT: 10ms → 500ms |
| **Routing Strategy** | Compare routing algorithm overhead | round-robin vs. least-connection |

### CI Integration

The benchmark framework integrates with GitHub Actions for automated regression testing:

```yaml
# .github/workflows/benchmark.yml (example)
benchmark:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - name: Create kind cluster
      run: ./hack/local-up-kthena.sh
    - name: Run benchmark suite
      run: |
        go run ./cmd/benchmark run \
          --scenario benchmarks/throughput-baseline.yaml \
          --output results/
    - name: Compare with baseline
      run: |
        go run ./cmd/benchmark compare \
          --current results/throughput-baseline.json \
          --baseline benchmarks/baselines/throughput-baseline.json
    - name: Fail on regression
      if: failure()
      run: echo "Performance regression detected. See benchmark report for details."
```

Key CI design decisions:
- Use **kind** (Kubernetes in Docker) for zero-cost ephemeral clusters
- Store baseline results as version-controlled JSON files in `benchmarks/baselines/`
- Fail the CI check when regression exceeds thresholds: >10% P95 latency increase or >10% max throughput decrease
- PR authors can update baselines by running the benchmark locally and committing the new JSON

### Optimization Strategy

The optimization phase follows a structured approach to prevent scope creep:

1. **Run benchmark suite** against the latest Router build
2. **Profile with pprof** (CPU + heap) during steady-state load to identify hotspots
3. **Classify hotspots** by category (CPU-bound, memory allocation, lock contention, I/O wait)
4. **Prioritize** up to 3 hotspots based on estimated impact and implementation effort
5. **Implement, benchmark, compare** — submit each optimization as a separate PR with before/after data

**Optimization targets** (candidates, to be confirmed by profiling):

| Category | Likely Hotspot | Optimization Approach |
|----------|---------------|----------------------|
| Memory | Repeated request body copies in proxy handler | Pool byte buffers or use `io.Copy` with pooled buffers |
| Lock contention | Global mutex on backend selection | Per-backend atomic counters or lock-free data structures |
| Connection pool | Default HTTP transport not tuned for LLM workloads | Tune `MaxIdleConnsPerHost`, idle timeout, disable HTTP/2 coalescing for backend connections |
| Serialization | Per-request JSON marshal/unmarshal in proxy path | Avoid re-serializing unchanged fields; use `json.RawMessage` pass-through |
| Goroutine | Unbounded goroutine creation for streaming connections | Introduce bounded worker pools for SSE relay |

**Fallback clause:** If profiling reveals no hotspots exceeding a 5% CPU/memory impact threshold, the optimization deliverable is replaced with:
- A performance characterization report documenting the Router's existing efficiency
- Recommendations for future optimization targets as traffic patterns evolve
- A "performance health" dashboard definition for ongoing monitoring

## Implementation Plan

### Phase 1: Framework Foundation (Weeks 1-4)

- Set up development environment (kind cluster via `local-up-kthena.sh`)
- Implement Mock LLM Backend with configurable latency profiles
- Implement Load Generator with SSE-aware timing and token counting
- Implement Benchmark Orchestrator CLI with YAML scenario parsing
- Implement Metrics Collector (loadgen metrics + Router Prometheus scraping)
- Implement Result Reporter (JSON + Markdown output)
- Write unit tests for all components
- Deliverable: Framework runs end-to-end on a local kind cluster

### Phase 2: Scenarios & CI (Weeks 5-6)

- Create all 8 pre-built benchmark scenario YAML files
- Integrate with GitHub Actions (kind cluster setup + benchmark execution + baseline comparison)
- Establish initial baseline results committed to the repository
- Write the runbook documenting the full test procedure
- Deliverable: CI pipeline runs benchmark suite on every PR to the Router

### Phase 3: Benchmark Report (Weeks 7-8)

- Run the full scenario suite and collect data
- Analyze results: latency distributions, throughput curves, resource utilization patterns
- Profile with pprof at representative load points
- Write the benchmark report with visualizations (tables, latency CDFs, throughput vs. QPS charts)
- Deliverable: Comprehensive benchmark report (Markdown + PDF)

### Phase 4: Optimization (Weeks 9-11)

- Identify up to 3 hotspots from profiling data
- Implement targeted optimizations (one PR per hotspot)
- Re-run benchmarks and produce before/after comparison data
- Community review, address feedback, merge
- Deliverable: Up to 3 optimization PRs with before/after data, OR performance characterization report (see fallback clause)

## Alternatives Considered

### 1. Extend the existing `benchmark/kthena-router/` Python tool
**Rejected.** The existing tool is a thin wrapper around `sglang/bench_serving.py`, written in Python, and designed for single-run ad-hoc testing. Rewriting in Go provides:
- Tighter integration with the Go-based Router (shared types, pprof integration)
- Better CI ergonomics (single Go binary, no Python dependency management)
- Full control over load generator behavior (SSE timing precision, rate control)

### 2. Use vegeta/hey/wrk as the load generator
**Rejected.** General-purpose HTTP load generators do not understand SSE streaming semantics (TTFT, TPOT, token counting) and cannot produce LLM-specific metrics. A purpose-built generator is necessary.

### 3. Require real GPU backends for all benchmarks
**Rejected.** GPU availability is not guaranteed across all developer machines and CI environments. Mock backends provide deterministic, reproducible results essential for regression detection. Real-backend benchmarks can be layered on top as an optional extension.

### 4. Build as a separate repository
**Rejected.** Keeping the benchmark framework in the kthena repository ensures:
- Co-versioning with the Router (benchmark and Router evolve together)
- Reuse of existing Go types, client libraries, and build tooling
- CI integration without cross-repo orchestration

## Open Questions

1. **Baseline storage strategy**: Should baseline results be stored as JSON files in the repo (simpler, but bloats repo size over time), or fetched from a GitHub Release artifact (cleaner, but more complex CI setup)?
2. **Multi-Router-instance benchmarks**: Should Phase 2 include scenarios with horizontally scaled Router instances behind a load balancer? This would measure the effectiveness of Router statelessness but adds significant CI complexity.
3. **Real-backend validation**: After the LFX term, should the framework add an optional "real backend" mode using vLLM/SGLang for validation that mock backend results correlate with real-world performance?
4. **Threshold tuning**: What regression thresholds should trigger CI failure? The initial proposal of >10% P95 latency or >10% max throughput needs community input based on production SLOs.

## References

- Kthena Router Benchmark Issue: https://github.com/volcano-sh/kthena/issues/942
- Existing benchmark tool: https://github.com/volcano-sh/kthena/tree/main/benchmark/kthena-router
- Kthena Architecture Docs: https://kthena.volcano.sh/docs/architecture
- sglang bench_serving: https://github.com/sgl-project/sglang/blob/main/python/sglang/bench_serving.py
- Go pprof Documentation: https://pkg.go.dev/net/http/pprof
- LFX Mentorship 2026 Term 2: https://github.com/cncf/mentoring/blob/main/programs/lfx-mentorship/2026/02-Jun-Aug/README.md
