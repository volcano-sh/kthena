---
slug: scoreplugin-benchmark-blog-post
title: Kthena Router ScorePlugin Architecture and Benchmark Analysis
authors: [bytebingo]
tags: []
---

# Kthena Router ScorePlugin Architecture and Benchmark Analysis

## Abstract

This paper analyzes the system design and implementation of the **ScorePlugin** module in **Kthena Router**, which leverages a configurable, pluggable architecture to enable multi-dimensional scoring and intelligent routing of inference requests. We provide a detailed examination of the six currently implemented **ScorePlugins**, and construct a standardized benchmarking environment based on the **DeepSeek-R1-Distill-Qwen-7B** model to evaluate the performance of different scheduling strategies under both long and short system prompt scenarios.

Experimental results demonstrate that in **long system prompt scenarios**, the **KVCacheAware Plugin + Least Request Plugin** combination achieves **2.73× higher throughput** and reduces **TTFT latency by 73.5%**, significantly optimizing overall inference service performance and validating the core value of cache-aware scheduling for large-scale model inference.

<!-- truncate -->

## 1. Introduction

In **LLM inference services**, intelligent request scheduling strategies have a decisive impact on overall system performance. Kthena Router adopts a **score-based scheduling framework** that enables multi-strategy load balancing through an **extensible plugin system**. This paper systematically analyzes the architectural design, core algorithms, and performance characteristics of this scheduling system.

## 2. System Architecture

### 2.1 Core Interface Design

The scheduling system in Kthena Router is built around a unified **ScorePlugin** interface:

```go
type ScorePlugin interface {
    Name() string
    Score(ctx *Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int
}
```

Each plugin generates a **normalized score in the range [0, 100]** for candidate Pods, where higher scores indicate better suitability for the incoming inference request.

### 2.2 Scheduling Execution Pipeline

The scheduler uses a **multi-stage pipeline architecture** consisting of the following steps:

1. **Filtering Phase** - Executes resource constraint checks and availability validation through Filter Plugins
2. **Scoring Phase** - Multiple Score Plugins calculate Pod suitability scores in parallel
3. **Weighted Aggregation** - Combines multi-plugin results according to configurable weights
4. **Selection Decision** - Selects the top-N Pods based on the final aggregated score

## 3. ScorePlugin Implementations

### 3.1 GPU Cache Usage Plugin

A resource-aware scheduling strategy based on **GPU cache utilization**.

**Algorithm Principles:**

- Monitors the **GPU cache usage rate** (`GPUCacheUsage`) of each Pod in real time
- Scoring function:
  ```
  Score = (1.0 - GPUCacheUsage) × 100
  ```
- Prioritizes Pods with **lower GPU cache utilization**, reducing the risk of memory overflow

**Use Case:** Environments with limited GPU memory availability.

### 3.2 Least Request Plugin

A load-balancing strategy based on **active request queue length**.

**Algorithm Principles:**

- **Metrics:**
  - Running request count: `RequestRunningNum`
  - Waiting queue length: `RequestWaitingNum`
- **Base load calculation:**
  ```
  base = RequestRunningNum + 100 × RequestWaitingNum
  ```
- **Normalized score:**
  ```
  score = ((maxScore - baseScores[info]) / maxScore) × 100
  ```

**Key Design Details:**

- Waiting queue weight factor set to **100**, strongly penalizing Pods with backlog accumulation
- Enables **dynamic load-aware balancing**

### 3.3 Least Latency Plugin

A performance-oriented scheduling strategy driven by **inference latency metrics**.

**Algorithm Principles:**

- **Metrics Monitored:**
  - **TTFT** (*Time To First Token*)
  - **TPOT** (*Time Per Output Token*)
- **Normalization:**
  ```
  ScoreTTFT = (MaxTTFT - CurrentTTFT) / (MaxTTFT - MinTTFT) × 100
  ScoreTPOT = (MaxTPOT - CurrentTPOT) / (MaxTPOT - MinTPOT) × 100
  ```
- **Final weighted score:**
  ```
  Score = α × ScoreTTFT + (1 - α) × ScoreTPOT
  ```

**Configuration:**

```yaml
TTFTTPOTWeightFactor: 0.5  # TTFT weight α
```

### 3.4 KVCacheAware Plugin

An advanced **cache-aware scheduling strategy** leveraging **KV cache hit rates**.

**Technical Highlights:**

- **Token-level semantic matching** with model-specific tokenizers
- **Distributed cache coordination** via Redis for cross-Pod state synchronization
- **Optimized contiguous block matching algorithm** to maximize cache hits

**Algorithm Workflow:**

1. **Tokenization Stage**
   ```
   Input → Model-specific Tokenizer → Token Sequence [t₁, t₂, ..., tₙ]
   ```

2. **Block Segmentation**
   ```
   Token Sequence → Fixed-size Blocks [B₁, B₂, ..., Bₘ]
   Block size: 128 tokens (configurable)
   ```

3. **Hash Generation**
   ```
   For each Bᵢ: Hash(Bᵢ) = SHA256(Bᵢ) → hᵢ
   ```

4. **Distributed Cache Query**
   ```
   Redis pipeline query:
   For each hᵢ → {Pod₁: timestamp₁, Pod₂: timestamp₂, ...}
   ```

5. **Contiguous Matching Score**
   ```
   Score = (Number of contiguous matching blocks / Total blocks) × 100
   ```

**Configuration Example:**

```yaml
blockSizeToHash: 128      # Token block size
maxBlocksToMatch: 128     # Maximum blocks to process
```

**Redis Key Structure:**

```
Key Pattern: "matrix:kv:block:{model}@{hash}"
Value Structure:
{
  "pod-name-1.namespace": "1703123456",
  "pod-name-2.namespace": "1703123789"
}
```

### 3.5 Prefix Cache Plugin

A **prefix-based cache-aware scheduling strategy**.

**Architecture Design:**

- **Three-level mapping**: `Model → Hash → Pod`
- **LRU-based cache eviction** for efficient memory management
- **Top-K prefix matching strategy** to return the top Pods with the longest prefix matches

**Key Features:**

- Byte-level prefix matching algorithm
- Optimized in-memory LRU implementation
- Configurable cache capacity and candidate size

### 3.6 Random Plugin

A **lightweight random load-balancing strategy**.

**Implementation:**

- Generates uniformly distributed random scores:
  ```
  Score ~ Uniform(0, 100)
  ```
- Thread-safe independent random number generator
- Stateless design ensures long-term balancing behavior

**Use Cases:**

- Mixed workloads without clear patterns
- Lightweight deployments avoiding complex scheduling overhead
- Establishing baseline performance comparisons

## 4. Experimental Design and Performance Evaluation

### 4.1 Experimental Setup

A standardized benchmarking environment was built using the **DeepSeek-R1-Distill-Qwen-7B** model to evaluate the performance of different scheduling strategies.

**Table 1: Experimental Environment Configuration**

| Parameter              | Value                                   |
| :--------------------- | :-------------------------------------- |
| Model                  | deepseek-ai/DeepSeek-R1-Distill-Qwen-7B |
| Block size             | 128                                     |
| Max model length       | 32,768                                  |
| Max batch token count  | 65,536                                  |
| Replicas               | 3                                       |
| GPU memory utilization | 0.9                                     |
| Max sequence count     | 256                                     |
| Dataset                | generated-shared-prefix                 |
| Request groups         | 256                                     |
| Requests per group     | 32                                      |
| Request rate           | 800 req/s                               |
| Max concurrency        | 300                                     |

### 4.2 Long System Prompt Scenario (4096 tokens)

**Table 2: Performance Metrics – Long Prompt**

| Plugin Configuration         | Runs  | Success Rate (%) | Throughput (req/s) | Latency (s) | TTFT (s) |
| :--------------------------- | :---: | :--------------: | :----------------: | :---------: | :------: |
| Least Request + KVCacheAware |   3   |      100.0       |     **32.22**      |  **9.22**   | **0.57** |
| Least Request + Prefix Cache |   3   |      100.0       |       23.87        |    12.47    |   0.83   |
| Random                       |   3   |      100.0       |       11.81        |    25.23    |   2.15   |
| Least Request                |   3   |      100.0       |        9.86        |    30.13    |  12.46   |
| GPU Usage                    |   3   |      100.0       |        9.56        |    30.92    |  13.14   |
| Least Latency                |   3   |      100.0       |        9.47        |    31.44    |  11.07   |

### 4.3 Short System Prompt Scenario (256 tokens)

**Table 3: Performance Metrics – Short Prompt**

| Plugin Configuration         | Runs  | Success Rate (%) | Throughput (req/s) | Latency (s) | TTFT (s) |
| :--------------------------- | :---: | :--------------: | :----------------: | :---------: | :------: |
| Least Request + Prefix Cache |   3   |      100.0       |        8.30        |    35.84    |  11.00   |
| Random                       |   3   |      100.0       |        8.21        |    36.25    |   4.54   |
| Least Request + KVCacheAware |   3   |      100.0       |        8.08        |    36.67    |  12.19   |
| Least Request                |   3   |      100.0       |        7.98        |    37.27    |  13.88   |
| GPU Usage                    |   3   |      100.0       |        7.77        |    38.15    |  15.03   |
| Least Latency                |   3   |      100.0       |        7.09        |    41.91    |  15.46   |

### 4.4 Visualization of Improvements

**Table 4: Performance Gains vs. Baseline (Random Plugin)**

| Plugin Configuration         | Throughput Gain | TTFT Improvement | Latency Reduction |
| :--------------------------- | :-------------: | :--------------: | :---------------: |
| Least Request + KVCacheAware |    **+173%**    |    **-73.5%**    |    **-63.5%**     |
| Least Request + Prefix Cache |      +102%      |      -61.4%      |      -50.6%       |

The **KVCacheAware combination strategy** shows **significant superiority** in long prefix scenarios, especially for throughput and TTFT metrics.

## 5. Results and Discussion

### 5.1 Advantages of Cache-Aware Strategies

Results clearly demonstrate the performance benefits of cache-aware scheduling for long prompts:

- **Peak throughput** of 32.22 req/s with **KVCacheAware + Least Request**, a **173% improvement** over the Random baseline
- **TTFT reduction** from 2.15s to 0.57s (**73.5% decrease**)
- **End-to-end latency** reduced from 25.23s to 9.22s (**63.5% improvement**)

### 5.2 Scenario Adaptability

**Long Prefix Scenario (4096 tokens):**

- Cache-aware strategies significantly outperform traditional balancing methods
- Token-level block matching maximizes cache utilization
- Distributed cache coordination prevents redundant computations across Pods

**Short Prefix Scenario (256 tokens):**

- Performance differences between strategies are minimal
- Limited prefix length restricts cache hit opportunities
- Traditional strategies provide stable results

### 5.3 Design Trade-Offs

**Computation Overhead vs. Performance Gains:**

- KVCacheAware introduces additional overhead from **tokenization** and **Redis queries**
- In high cache-hit environments, performance gains far outweigh the added cost
- Recommends **adaptive strategy selection** based on workload characteristics

## 6. Deployment Recommendations

### 6.1 High Cache Hit Scenarios

**Recommended Configuration:** `KVCacheAware Plugin + Least Request Plugin`

**Use Cases:**

- Customer service chatbots, code generation, and similar structured workloads
- Multi-turn conversations with long system prompts
- Template-based content generation services

### 6.2 General Load-Balancing Scenarios

**Recommended Configuration:** `Least Request Plugin + Least Latency Plugin`

**Use Cases:**

- General-purpose inference services with diverse request patterns
- Multi-tenant environments requiring fair resource distribution
- Real-time interactive applications

### 6.3 Resource-Constrained Environments

**Recommended Configuration:** `GPU Cache Usage Plugin`

**Use Cases:**

- Edge deployments with limited GPU memory
- Cost-sensitive cloud environments
- Scenarios where multiple models share GPU resources

## 7. Conclusion

This paper presents a comprehensive analysis of the Kthena Router ScorePlugin architecture and its performance characteristics. The experimental results validate the effectiveness of cache-aware scheduling strategies, particularly in scenarios with long system prompts where the **KVCacheAware Plugin + Least Request Plugin** combination achieves substantial performance improvements.

Key findings include:

1. **Cache-aware strategies provide significant performance benefits** in long prompt scenarios, with up to 173% throughput improvement
2. **Adaptive strategy selection** based on workload characteristics is crucial for optimal performance
3. **The pluggable architecture** enables flexible deployment configurations tailored to specific use cases

Future work will focus on developing adaptive scheduling algorithms that can automatically select optimal plugin combinations based on real-time workload analysis and system conditions.

---

*Document generated on: September 9, 2025*
