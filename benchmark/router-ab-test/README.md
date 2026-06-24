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
│  │ • QPS 控制   │      │ • 路由决策         │      │ • TTFT 模拟      │   │
│  │ • 并发控制    │      │ • 连接池管理       │      │ • TPOT 模拟      │   │
│  │ • 到达分布    │      │ • 负载均衡         │      │ • KV Cache 模拟  │   │
│  │ • 追踪回放    │      │ • 故障转移         │      │ • Prometheus 指标│   │
│  └──────────────┘      └──────────────────┘      └──────────────────┘   │
│         │                      │                         │              │
│         └──────────────────────┼─────────────────────────┘              │
│                                │                                        │
│                         Metrics Collector                               │
│                    (AIPerf 内置 + Router Prometheus)                     │
└─────────────────────────────────────────────────────────────────────────┘
```

### 为什么选择 AIPerf + Dynamo Mocker

| 组件 | 选择理由 |
|------|----------|
| **AIPerf** | NVIDIA 官方工具，支持多种到达模式（Poisson/Gamma/Constant）、Credit-Based 流控、实时 TUI Dashboard、详细的 TTFT/TPOT 指标 |
| **Dynamo Mocker** | GPU-free 的高保真 LLM 推理模拟，支持 vLLM/SGLang 两种引擎模式、KV Cache 模拟、Prefix Caching、可配置延迟模型 |
| **K8s 部署** | 复用 kthena 现有 Helm Charts 和 CRD 定义，与 E2E 测试基础设施一致 |

## 快速开始

### 前置条件

- Docker Desktop 或 Podman
- Kind (Kubernetes in Docker)
- Helm 3.x
- kubectl
- Python 3.10+

### 1. 创建 Kind 集群并部署 Kthena

```bash
# 使用 kthena 提供的脚本创建本地集群
cd /path/to/kthena
./hack/local-up-kthena.sh
```

### 2. 部署 Mock Backend

```bash
cd benchmark/router-ab-test

# 部署 4 个 homogeneous mocker pods (TTFT=50ms, TPOT=15ms)
kubectl apply -f k8s/mocker-deployment.yaml

# 等待 pods 就绪
kubectl wait --for=condition=ready pod -l app=mocker-llm --timeout=120s
```

### 3. 部署 Kthena Router

```bash
# 部署 ModelServer + ModelRoute
kubectl apply -f k8s/modelserver.yaml
kubectl apply -f k8s/modelroute.yaml
```

### 4. 运行 A/B 测试

```bash
# 安装 AIPerf
pip install aiperf

# 运行 A/B 测试（比较 random vs least-latency 路由器调度配置）
python scripts/ab_test.py \
    --scenario scenarios/smoke-test-s2.yaml \
    --router-config-a k8s/router-config-random.yaml \
    --router-config-b k8s/router-config-least-latency.yaml \
    --output results/
```

## 目录结构

```
router-ab-test/
├── README.md                    # 本文档
├── k8s/                         # K8s 资源定义
│   ├── mocker-deployment.yaml   # Dynamo Mocker Deployment
│   ├── mocker-service.yaml      # Mocker Service
│   ├── modelserver.yaml         # Kthena ModelServer CRD
│   ├── modelroute.yaml           # Kthena ModelRoute CRD (routes to mocker ModelServer)
│   ├── router-config-*.yaml      # Router scheduler ConfigMap（不同策略）
│   └── aiperf-job.yaml          # AIPerf K8s Job 模板
├── scenarios/                   # 测试场景配置
│   ├── smoke-test-s1.yaml       # S1: Throughput Baseline
│   ├── smoke-test-s2.yaml       # S2: Latency vs QPS
│   └── ...
├── scripts/                     # 测试脚本
│   ├── ab_test.py               # A/B 测试编排器
│   ├── collect_metrics.py       # 指标收集
│   └── generate_report.py       # 报告生成
└── results/                     # 测试结果输出目录
```

## 场景配置

场景配置遵循三明治模型，分为三部分，以 S2 为例：

```yaml
name: "latency-vs-qps-50qps"
description: "50 QPS 下的延迟基线测试"

# 左侧：用户流量（AIPerf 配置）
load:
  schedule:
    mode: "rate"
    rate: 50
  traffic:
    burstiness: 1.0        # Poisson 到达
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

# 右侧：后端响应（Mocker 配置）
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

# 中间：路由策略
routing:
  strategy: "least-latency"
```

## A/B 测试流程

```
┌─────────────────────────────────────────────────────────────────┐
│                    A/B Test Orchestrator                         │
├─────────────────────────────────────────────────────────────────┤
│  1. 部署 Mock Backend (如果尚未部署)                             │
│  2. 应用 Config A (如 random)                               │
│  3. 等待 Router 重新加载配置                                     │
│  4. 运行 AIPerf 负载测试 → 收集 Metrics A                       │
│  5. 应用 Config B (如 least-latency)                             │
│  6. 等待 Router 重新加载配置                                     │
│  7. 运行 AIPerf 负载测试 → 收集 Metrics B                       │
│  8. 生成对比报告                                                 │
└─────────────────────────────────────────────────────────────────┘
```

## 测试场景

一共设计了 8 大场景：

| # | 场景 | 验证目标 | 关键参数 |
|---|------|----------|----------|
| S1 | Throughput Baseline | 最大可持续吞吐量 | 逐步增加 QPS 直到 50% 错误率 |
| S2 | Latency vs QPS | 不同负载下的路由开销 | QPS: 10, 50, 100, 200, 500 |
| S3 | Concurrency Scaling | 连接池行为 | Connections: 10, 100, 500, 1000 |
| S4 | Backend Count Impact | 调度器随 pod 数扩展 | Backends: 1, 4, 16, 32 |
| S5 | Prompt Length Impact | 请求体解析开销 | Prompt tokens: 100, 1000, 4000 |
| S6 | Long Response | SSE 中继开销 | Response tokens: 100, 1000, 4096 |
| S7 | Backend Latency Variance | 异构后端调度行为 | 3 pods: TTFT 10/100/500ms |
| S8 | Routing Strategy Comparison | 路由策略开销对比 | random vs least-latency vs least-request |

## 指标说明

### AIPerf 输出指标

| 指标 | 说明 |
|------|------|
| Time to First Token (TTFT) | 从发送请求到收到第一个 token 的时间 |
| Time Per Output Token (TPOT) | 相邻两个 output token 之间的间隔 |
| Request Latency | 请求总延迟 |
| Output Token Throughput | 每秒输出 token 数 |
| Request Throughput | 每秒完成请求数 |

### Router Prometheus 指标

| 指标 | 说明 |
|------|------|
| `scheduler_plugin_duration_ms` | 调度插件执行耗时 |
| `active_upstream_requests` | 活跃上游请求数 |
| `fairness_queue_depth` | 公平队列深度 |

## 备忘

准备向上游提交 PR 时，请执行:

```shell
./hack/manage-workflows.sh enable
git add .github/workflows/
git commit -m "Restore all workflows"
```

## 参考文档

- [Kthena Router Benchmark Proposal](../../docs/proposal/router-benchmark.md)
- [AIPerf Documentation](https://github.com/ai-dynamo/aiperf)
- [Dynamo Mocker Documentation](https://github.com/ai-dynamo/dynamo/blob/main/docs/mocker/mocker.md)
- [Kthena Architecture](../../docs/kthena/docs/architecture/)
