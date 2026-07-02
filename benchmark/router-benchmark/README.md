# Kthena Router A/B Test Framework

基于"三明治模型"的 Kthena Router 性能基准测试框架，使用 AIPerf 作为负载生成器，Dynamo Mocker 作为模拟后端。

## 架构设计

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Sandwich Isolation Model                         │
│                                                                         │
│  ┌──────────────┐      ┌──────────────────┐      ┌──────────────────┐   │
│  │   AIPerf     │      │  Kthena Router   │      │  Dynamo Mocker   │   │
│  │  (Load Gen)  │─────►│   (Under Test)   │─────►│  (Mock Backend)  │   │
│  │              │      │                  │      │                  │   │
│  │ • QPS 控制   │      │ • 路由决策       │      │ • TTFT 模拟      │   │
│  │ • 并发控制   │      │ • 连接池管理     │      │ • TPOT 模拟      │   │
│  │ • 到达分布   │      │ • 负载均衡       │      │ • KV Cache 模拟  │   │
│  │ • 追踪回放   │      │ • 故障转移       │      │ • Prometheus 指标│   │
│  └──────────────┘      └──────────────────┘      └──────────────────┘   │
│         │                      │                         │              │
│         └──────────────────────┼─────────────────────────┘              │
│                                │                                        │
│                         Metrics Collector                               │
│               (AIPerf Output + Router Prometheus + Router pprof)        │
└─────────────────────────────────────────────────────────────────────────┘
```

### 为什么选择 AIPerf + Dynamo Mocker

| 组件 | 选择理由 |
|------|----------|
| AIPerf | NVIDIA 官方工具，支持多种到达模式（Poisson/Gamma/Constant）、Credit-Based 流控、实时 TUI Dashboard、详细的 TTFT/TPOT 指标 |
| Dynamo Mocker | GPU-free 的高保真 LLM 推理模拟，支持 vLLM/SGLang 两种引擎模式、KV Cache 模拟、Prefix Caching、可配置延迟模型 |
| K8s 部署 | 复用 kthena 现有 Helm Charts 和 CRD 定义，与 E2E 测试基础设施一致 |

## 当前模块化结构

为了让当前的 `ab_test` 更容易 review，并逐步对齐 proposal 中的分层设计，脚本已经拆为如下结构：

```
router-ab-test/
├── README.md
├── k8s/
│   ├── mocker-deployment.yaml
│   ├── modelroute.yaml
│   ├── modelserver.yaml
│   ├── router-config-least-latency.yaml
│   ├── router-config-least-request.yaml
│   └── router-config-random.yaml
├── scenarios/
│   ├── smoke-test-s2.yaml
│   ├── smoke-test-s7.yaml
│   └── smoke-test-s8.yaml
├── scripts/
│   ├── ab_test.py                         # CLI 入口
│   └── router_ab_test/
│       ├── __init__.py                    
│       ├── models.py                      # ScenarioConfig / BenchmarkResult
│       ├── kubernetes.py                  # K8sManager：apply、rollout、probe、port-forward
│       ├── load_generator.py              # AIPerfRunner：scenario -> aiperf CLI
│       ├── metrics_collector.py           # MetricsCollector：Prometheus / pprof 采集
│       ├── orchestrator.py                # ABTestOrchestrator：执行 A/B 流程
│       └── reporter.py                    # ResultReporter：compare / write / print report
└── tests/
    └── test_ab_test.py                    
```

### 模块职责

- `scripts/ab_test.py`
  - 保留原始入口路径，避免已有命令、文档或测试失效
  - 提供 CLI parser 和 `main()`
- `scripts/router_ab_test/models.py`
  - benchmark 领域模型与场景配置加载
  - `ScenarioConfig.metrics` 定义每个场景是否采集 Prometheus / pprof
  - `BenchmarkResult.artifacts` 保存额外采集结果
- `scripts/router_ab_test/kubernetes.py`
  - 负责 Kubernetes 资源操作、router rollout、service/debug 端口转发、route ready probe
- `scripts/router_ab_test/load_generator.py`
  - 负责把 scenario YAML 映射为 AIPerf CLI 参数
- `scripts/router_ab_test/metrics_collector.py`
  - 负责抓取 router `/metrics`
  - 负责抓取 router `/debug/pprof/profile` 与其他 profile
  - 负责把指标与 profile 文件写入 `artifacts/<config>/`
- `scripts/router_ab_test/orchestrator.py`
  - 负责串联 A/B 执行流程
  - 在每轮 AIPerf 结束后触发 Metrics Collector
- `scripts/router_ab_test/reporter.py`
  - 负责比较指标、生成报告结构、写 JSON、打印 summary

## 快速开始

### 前置条件

- Docker Desktop 或 Podman
- Kind (Kubernetes in Docker)
- Helm 3.x
- kubectl
- Python 3.10+

### 1. 创建 Kind 集群并部署 Kthena

```bash
cd /path/to/kthena
./hack/local-up-kthena.sh
```

### 2. 部署 Mock Backend

```bash
kubectl apply -f k8s/mocker-deployment.yaml # HF 无法访问，可使用 k8s/mocker-deployment-local-hf.yaml
kubectl wait --for=condition=ready pod -l app=mocker-llm --timeout=120s
```

### 3. 部署 Kthena Router 相关模型资源

```bash
kubectl apply -f k8s/modelserver.yaml
kubectl apply -f k8s/modelroute.yaml
```

### 4. 安装 AIPerf

```bash
pip install 'aiperf>=0.9,<0.11'
```

### 5. 运行 A/B 测试

```bash
python scripts/ab_test.py \
  --scenario scenarios/smoke-test-s2.yaml \
  --router-config-a k8s/router-config-random.yaml \
  --router-config-b k8s/router-config-least-latency.yaml \
  --output /tmp/results/
```

#### Router Endpoint 访问模式

支持两种访问 Router 端点的模式，通过 `--endpoint-mode` 参数选择：

| 模式       | 适用场景      | 说明                                                                        |
|----------|-----------|---------------------------------------------------------------------------|
| `pf`（默认） | Kind 测试集群 | 使用 `kubectl port-forward` 将 Router Service 端口转发到 `localhost:<local_port>` |
| `lb`     | 生产集群      | 从 LoadBalancer 获取 EXTERNAL-IP，组成 `<external_ip>:<node_port>`              |

> 备注：Debug port (15000) 未通过 Service 暴露，两种模式下均使用 port-forward。

## 场景配置

场景配置遵循三明治模型，分为三部分。以 `smoke-test-s2.yaml` 为例：

```yaml
name: "smoke-test-s2-latency-vs-qps"
description: "s2 scenario: routing latency under different QPS"

load:
  schedule:
    mode: "rate"
    rate: 50
  traffic:
    burstiness: 1.0
    ramp:
      strategy: "none"
  concurrency:
    connections: 100
  prompts:
    - tokens: 512
      weight: 10
  max_tokens:
    - tokens: 128
      weight: 10
  duration: "60s"

backends:
  count: 4
  profiles:
    - name: "homogeneous"
      count: 4
      ttftMean: "50ms"
      ttftStddev: "10ms"
      tpotMean: "15ms"
      tpotStddev: "3ms"
  responseTokens: 128
  errorRate: 0.0

routing:
  strategy: "least-latency"

metrics:
  prometheus: true
  pprof: true
  cpuProfileSeconds: 15
  profiles:
    - heap
    - goroutine
    - allocs
    - mutex

aiperf:
  extraArgs:
    - "--tokenizer"
    - "Qwen/Qwen3-0.6B"
```

其中：

- `metrics.prometheus: true`
  - 采集 `http://<router-endpoint>/metrics`
- `metrics.pprof: true`
  - 采集 `http://<router-debug-endpoint>/debug/pprof/*`

## A/B 测试流程

```
1. Apply router config A
2. Restart and wait for router rollout
3. Wait for backend deployment ready
4. Start kubectl port-forward to router service
5. Start kubectl port-forward to router debug port
6. Probe /v1/chat/completions until the route is really warm
7. Run AIPerf and collect result A
8. Scrape router /metrics and fetch pprof artifacts for config A
9. Apply router config B
10. Repeat warmup + benchmark + metrics collection for config B
11. Compare metrics and write report_<scenario>.json
12. Exit non-zero if report contains regression
```

## 输出结果

结果会写入 `--output` 指定目录。当前输出包括：

- `runs/config_a/` 与 `runs/config_b/`
  - AIPerf 原始输出目录
- `artifacts/config_a/` 与 `artifacts/config_b/`
  - Router Prometheus 与 pprof 采集结果
- `report_<scenario>.json`
  - A/B 对比结果

## 测试场景

一共设计了 8 大场景：

| # | 场景 | 验证目标 | 关键参数 |
|---|------|----------|----------|
| S1 | Throughput Baseline | 最大可持续吞吐量 | 逐步增加 QPS |
| S2 | Latency vs QPS | 不同负载下的路由开销 | QPS: 10, 50, 100, 200, 500 |
| S3 | Concurrency Scaling | 连接池行为 | Connections: 10, 100, 500, 1000 |
| S4 | Backend Count Impact | 调度器随 pod 数扩展 | Backends: 1, 4, 16, 32 |
| S5 | Prompt Length Impact | 请求体解析开销 | Prompt tokens: 100, 1000, 4000 |
| S6 | Long Response | SSE 中继开销 | Response tokens: 100, 1000, 4096 |
| S7 | Backend Latency Variance | 异构后端调度行为 | 3 pods: TTFT 10/100/500ms |
| S8 | Routing Strategy Comparison | 路由策略开销对比 | random vs least-latency vs least-request |

## 参考文档

- [Kthena Router Benchmark Proposal](../../docs/proposal/kthena-router-benchmark.md)
- [AIPerf Documentation](https://github.com/ai-dynamo/aiperf)
- [Dynamo Mocker Documentation](https://github.com/ai-dynamo/dynamo/blob/main/docs/mocker/mocker.md)
- [Kthena Architecture](../../docs/kthena/docs/architecture/)
