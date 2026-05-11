# Proposal: KV Cache Aware Plugin for Kthena Router Scheduler

## Goals

- **Token-based Block Matching**: Implement intelligent pod scheduling based on KV cache hit potential using token-level block matching
- **Distributed Cache Coordination**: Leverage Redis for cross-pod cache coordination in distributed inference environments
- **Advanced Tokenization Support**: Integrate with model-specific tokenizers and chat template processing for accurate token sequence handling
- **Semantic Cache Alignment**: Use token-based blocks instead of byte-based blocks for better semantic alignment with model inference patterns

## 1. Introduction

The KV Cache Aware Plugin is a scoring plugin for the Kthena router scheduler that implements intelligent pod scheduling based on KV cache hit potential. Unlike traditional prefix cache approaches that use byte-based matching, this plugin leverages token-level block matching with Redis-based distributed coordination to optimize inference performance in multi-pod environments.

The plugin addresses the challenge of efficiently routing inference requests to pods that are most likely to have relevant KV cache entries, thereby reducing computation overhead and improving response times for similar or related prompts.

## 2. Architecture Overview

### 2.1. Core Components

**KVCacheAware Plugin**
- Main plugin implementing the `framework.ScorePlugin` interface
- Manages distributed caching mechanism using Redis
- Integrates with tokenization system for accurate token processing
- Provides configurable parameters for performance tuning

**TokenBlockProcessor**
- Processes token sequences into fixed-size blocks for hashing
- Generates SHA-256 based hashes for consistent block identification
- Handles token chunking with configurable block sizes

**Tokenization Integration**
- TokenizerManager for model-specific tokenizer management
- Support for vLLM remote tokenization
- Chat template processing for ChatML format requests

**Redis-based Distributed Cache**
- Uses Redis hash structures for block-to-pod mappings
- Efficient pipeline operations for batch queries
- Timeout handling and error recovery

### 2.2. Key Features

**Token Block Matching**
- Tokenizes input prompts using model-specific tokenizers
- Divides token sequences into fixed-size blocks (default: 128 tokens)
- Generates standardized SHA-256 hashes for each token block
- Queries Redis to find pods with cached token blocks

**Chat Template Support**
- Automatic detection of chat completion requests
- ChatML format processing with role and content extraction
- Integration with model-specific chat templates

**Scoring Mechanism**
- Scores pods based on consecutive token block matches from the beginning
- Score calculation: `(matching consecutive blocks / total blocks) * 100`
- Range: 0-100, higher scores indicate better KV cache hit potential
- Early termination when no pods have consecutive matches

## 3. Technical Implementation

### 3.1. Token Processing Flow

```
Input Prompt → Tokenization → Block Division → Hash Generation → Redis Query → Pod Scoring
```

1. **Tokenization**: Convert input text/messages to token sequences using model-specific tokenizers
2. **Block Division**: Split tokens into fixed-size blocks (configurable, default 128)
3. **Hash Generation**: Generate SHA-256 hashes for each token block
4. **Redis Query**: Batch query Redis for pods that have cached each block
5. **Pod Scoring**: Calculate scores based on consecutive block matches

### 3.2. Redis Data Structure

**Key Format**: `matrix:kv:block:{model}@{hash}`

**Example**:
```
Key: "matrix:kv:block:deepseek-ai/DeepSeek-R1-Distill-Qwen-7B@12345678901234567890"
Fields: {
  "pod-name-1.namespace.svc.cluster.local": "1703123456",
  "pod-name-2.namespace.svc.cluster.local": "1703123789"
}
```

### 3.3. Configuration Parameters

```yaml
# KVCacheAware configuration
blockSizeToHash: 16       # Tokens per block for hashing
maxBlocksToMatch: 128     # Maximum blocks to process
```

### 3.4. Scoring Algorithm

The plugin implements a consecutive block matching algorithm:

1. **First Block Filtering**: Only consider pods that have the first token block
2. **Consecutive Matching**: For each subsequent block, only keep pods that have both the current and all previous blocks
3. **Early Termination**: Stop processing when no pods have consecutive matches
4. **Score Calculation**: `(consecutive_matches / total_blocks) * 100`

## 4. Integration with Kthena

### 4.1. Scheduler Framework Integration

The plugin integrates with the Kthena scheduler framework as a scoring plugin:

```go
var _ framework.ScorePlugin = &KVCacheAware{}

func (t *KVCacheAware) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
    // Implementation returns scores 0-100 for each pod
}
```

### 4.2. Tokenizer Integration

The plugin leverages the existing tokenization infrastructure:

- **TokenizerManager**: Manages model-specific tokenizers
- **vLLM Remote Support**: Integrates with vLLM tokenization endpoints
- **Chat Template Processing**: Handles ChatML format with proper role extraction

### 4.3. Redis Integration

Uses the existing Redis infrastructure:

- **Singleton Pattern**: Leverages `utils.TryGetRedisClient()` for connection management
- **Pipeline Operations**: Efficient batch queries for multiple blocks
- **Error Handling**: Graceful degradation when Redis is unavailable

## 5. Performance Considerations

### 5.1. Optimization Strategies

**Block Size Tuning**
- Larger blocks: Fewer Redis queries, less granular matching
- Smaller blocks: More Redis queries, more granular matching
- Default 128 tokens balances performance and accuracy

**Maximum Block Limits**
- Prevents excessive processing for very long prompts
- Default limit of 128 blocks covers most practical use cases
- Configurable based on deployment requirements

**Pipeline Queries**
- Batch Redis operations to minimize network overhead
- Single pipeline query for all blocks in a request
- Timeout handling to prevent blocking

## 6. Usage Scenarios

The KV Cache Aware Plugin is particularly effective for:

- **Chat Completion Workloads**: Similar conversation patterns with shared context
- **Text Completion Tasks**: Repeated prompt prefixes across requests
- **Multi-turn Conversations**: Context reuse in conversational AI
- **Template-based Generation**: Requests with common prompt templates

## 7. Conclusion

The KV Cache Aware Plugin provides a sophisticated approach to pod scheduling based on KV cache hit potential. By leveraging token-level block matching with distributed Redis coordination, it enables intelligent request routing that can significantly improve inference performance in multi-pod environments.

The plugin's integration with advanced tokenization capabilities and chat template processing makes it particularly well-suited for modern LLM inference workloads, while its configurable parameters allow for optimization based on specific deployment requirements and performance characteristics.
