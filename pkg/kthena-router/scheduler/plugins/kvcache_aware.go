/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
KV Cache Aware Plugin

The KV Cache Aware Plugin is a scoring plugin for the Kthena router scheduler that implements
intelligent pod scheduling based on KV cache hit potential using token-level block matching
with Redis-based distributed coordination.

For detailed design documentation, architecture overview, and implementation details,
see: docs/proposal/kvcache-aware-plugin-design.md
*/

package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/tokenization"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

const (
	// KVCacheAwarePluginName is the name identifier for the KV cache scoring plugin
	KVCacheAwarePluginName = "kvcache-aware"

	// kvCacheKeyPrefix is the Redis key prefix for storing token block mappings
	// Redis key format: "matrix:kv:block:{model}@{hash}"
	// Example: "matrix:kv:block:deepseek-ai/DeepSeek-R1-Distill-Qwen-7B@12345678901234567890"
	kvCacheKeyPrefix = "matrix:kv:block:"

	// defaultBlockSizeToHash is the default number of tokens per block for hashing
	// Each token sequence is divided into blocks of this size before generating hashes
	defaultBlockSizeToHash = 128

	// defaultMaxBlocksToMatch is the default maximum number of blocks to process for scoring
	// Limits the number of blocks to prevent excessive Redis queries and processing time
	defaultMaxBlocksToMatch = 128
)

type KVCacheAwareArgs struct {
	BlockSizeToHash  int `yaml:"blockSizeToHash,omitempty"`
	MaxBlocksToMatch int `yaml:"maxBlocksToMatch,omitempty"`
}

type KVCacheAware struct {
	name             string
	maxBlocksToMatch int
	keyPrefix        string
	redisClient      *redis.Client
	processor        *TokenBlockProcessor
	tokenizerManager *tokenization.TokenizerManager
}

var _ framework.ScorePlugin = &KVCacheAware{}

type TokenBlockProcessor struct {
	blockSize int
}

// KVCacheAwareBlock represents a token block for Redis storage
type KVCacheAwareBlock struct {
	ModelName string // Model name (e.g., "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B")
	ChunkHash uint64 // SHA-256 hash of the token block
}

// String generates the Redis key for this token block
// Format: "{prefix}{model}@{hash}"
// Example: "matrix:kv:block:deepseek-ai/DeepSeek-R1-Distill-Qwen-7B@12345678901234567890"
//
// The resulting Redis hash structure:
//
//	Key: "matrix:kv:block:deepseek-ai/DeepSeek-R1-Distill-Qwen-7B@12345678901234567890"
//	Fields: {
//	  "pod-name-1.namespace": "1703123456",
//	  "pod-name-2.namespace": "1703123789"
//	}
func (b KVCacheAwareBlock) String(prefix string) string {
	return fmt.Sprintf("%s%s@%d", prefix, b.ModelName, b.ChunkHash)
}

func NewKVCacheAware(pluginArg runtime.RawExtension) *KVCacheAware {
	var args KVCacheAwareArgs
	if len(pluginArg.Raw) > 0 {
		if err := yaml.Unmarshal(pluginArg.Raw, &args); err != nil {
			klog.Warningf("Failed to unmarshal KVCacheAwareArgs: %v", err)
		}
	}

	blockSizeToHash := args.BlockSizeToHash
	if blockSizeToHash <= 0 {
		blockSizeToHash = defaultBlockSizeToHash
	}
	maxBlocksToMatch := args.MaxBlocksToMatch
	if maxBlocksToMatch <= 0 {
		maxBlocksToMatch = defaultMaxBlocksToMatch
	}

	managerConfig := tokenization.TokenizerManagerConfig{
		EnableVLLMRemote: true,
		EndpointTemplate: "http://%s:8000",
	}
	manager := tokenization.NewTokenizerManager(managerConfig)

	redisClient := utils.TryGetRedisClient()

	return &KVCacheAware{
		name:             KVCacheAwarePluginName,
		maxBlocksToMatch: maxBlocksToMatch,
		keyPrefix:        kvCacheKeyPrefix,
		redisClient:      redisClient,
		processor:        &TokenBlockProcessor{blockSize: blockSizeToHash},
		tokenizerManager: manager,
	}
}

func (t *KVCacheAware) Name() string {
	return t.name
}

func (t *KVCacheAware) normalizeAndTokenizePrompt(ctx *framework.Context, pods []*datastore.PodInfo) ([]uint32, error) {
	if t.tokenizerManager == nil {
		return nil, fmt.Errorf("tokenizer manager not available")
	}
	return t.tokenizerManager.TokenizePrompt(ctx.Model, ctx.Prompt, pods)
}

func (t *KVCacheAware) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scoreResults := make(map[*datastore.PodInfo]int)

	for _, pod := range pods {
		scoreResults[pod] = 0
	}

	if (ctx.Prompt.Text == "" && len(ctx.Prompt.Messages) == 0) || ctx.Model == "" {
		return scoreResults
	}

	start := time.Now()
	tokens, err := t.normalizeAndTokenizePrompt(ctx, pods)
	tokenizerDuration := time.Since(start)
	klog.V(4).Infof("Tokenizer processing time: %v", tokenizerDuration)

	if err != nil || len(tokens) == 0 {
		return scoreResults
	}

	blockHashes := t.processor.TokensToBlockHashes(tokens, t.maxBlocksToMatch)
	if len(blockHashes) == 0 {
		return scoreResults
	}

	blockToPods, err := t.queryRedisForBlocks(blockHashes, ctx.Model)
	if err != nil {
		return scoreResults
	}

	podScores := t.calculatePodScores(blockHashes, blockToPods)

	for _, pod := range pods {
		podName := pod.Pod.Name
		if score, exists := podScores[podName]; exists {
			scoreResults[pod] = score
		}
	}
	return scoreResults
}

// queryRedisForBlocks queries Redis to find which pods have cached the given token block hashes
// Returns a map from block hash to list of pod names that have cached that block
func (t *KVCacheAware) queryRedisForBlocks(blockHashes []uint64, modelName string) (map[uint64][]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	blockToPods := make(map[uint64][]string)

	if t.redisClient == nil {
		return blockToPods, fmt.Errorf("redis client not initialized")
	}

	pipe := t.redisClient.Pipeline()
	cmds := make([]*redis.StringSliceCmd, len(blockHashes))

	// Build pipeline commands for batch Redis query
	for i, hash := range blockHashes {
		block := KVCacheAwareBlock{ModelName: modelName, ChunkHash: hash}
		key := block.String(t.keyPrefix)
		cmds[i] = pipe.HKeys(ctx, key)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}

	// Process results and extract pod names
	for i, cmd := range cmds {
		pods, err := cmd.Result()
		if err != nil || len(pods) == 0 {
			continue
		}

		podNames := make([]string, 0, len(pods))
		for _, pod := range pods {
			// Redis field is pod identifier (e.g., "pod-name.namespace")
			podName := extractPodNameFromIdentifier(pod)
			podNames = append(podNames, podName)
		}
		blockToPods[blockHashes[i]] = podNames
	}

	return blockToPods, nil
}

func extractPodNameFromIdentifier(podIdentifier string) string {
	if idx := strings.IndexByte(podIdentifier, '.'); idx >= 0 {
		return podIdentifier[:idx]
	}
	return podIdentifier
}

func (t *KVCacheAware) calculatePodScores(blockHashes []uint64, blockToPods map[uint64][]string) map[string]int {
	podScores := make(map[string]int)

	if len(blockHashes) == 0 {
		klog.Infof("KVCacheAware: No block hashes to process")
		return podScores
	}

	firstBlockPods, exists := blockToPods[blockHashes[0]]
	if !exists || len(firstBlockPods) == 0 {
		return podScores
	}

	activePods := make(map[string]bool, len(firstBlockPods))
	for _, podName := range firstBlockPods {
		activePods[podName] = true
		podScores[podName] = 1
	}

	for i := 1; i < len(blockHashes); i++ {
		if len(activePods) == 0 {
			break
		}

		blockPods, exists := blockToPods[blockHashes[i]]
		if !exists || len(blockPods) == 0 {
			break
		}

		nextActivePods := make(map[string]bool)
		for _, podName := range blockPods {
			if activePods[podName] {
				nextActivePods[podName] = true
				podScores[podName]++
			}
		}

		if len(nextActivePods) == 0 {
			break
		}

		activePods = nextActivePods
	}

	totalBlocks := len(blockHashes)
	for podName, matchLen := range podScores {
		score := int((float64(matchLen) / float64(totalBlocks)) * 100)
		podScores[podName] = score
		klog.V(4).Infof("KVCacheAware Pod %s: matched %d/%d blocks, score: %d", podName, matchLen, totalBlocks, score)
	}

	return podScores
}

func (tbp *TokenBlockProcessor) TokensToBlockHashes(tokens []uint32, maxBlocks int) []uint64 {
	if len(tokens) == 0 {
		return nil
	}

	chunks := tbp.chunkTokens(tokens, maxBlocks)
	return tbp.computeBlockHashes(chunks)
}

// computeStandardizedHash generates a consistent hash for token sequences using SHA-256
// Returns a 63-bit positive integer for Redis/database compatibility
func computeStandardizedHash(tokenIds []uint32) uint64 {
	if len(tokenIds) == 0 {
		return 0
	}

	h := sha256.New()
	var tokenBytes [4]byte
	for _, tokenId := range tokenIds {
		binary.BigEndian.PutUint32(tokenBytes[:], tokenId)
		h.Write(tokenBytes[:])
	}

	hashBytes := h.Sum(nil)
	fullHash := binary.BigEndian.Uint64(hashBytes[:8])

	// Clear MSB to ensure positive value (0x7FFFFFFFFFFFFFFF masks out sign bit)
	result := fullHash & 0x7FFFFFFFFFFFFFFF
	klog.V(4).Infof("KVCacheAware: compute standardized hash - token_ids=%v, hash=%d", tokenIds, result)
	return result
}

func (tbp *TokenBlockProcessor) chunkTokens(tokens []uint32, maxBlocks int) [][]uint32 {
	numBlocks := (len(tokens) + tbp.blockSize - 1) / tbp.blockSize
	if numBlocks > maxBlocks {
		numBlocks = maxBlocks
	}
	chunks := make([][]uint32, 0, numBlocks)
	for i := 0; i < len(tokens) && len(chunks) < maxBlocks; i += tbp.blockSize {
		end := i + tbp.blockSize
		if end > len(tokens) {
			end = len(tokens)
		}
		chunks = append(chunks, tokens[i:end])
	}
	return chunks
}

func (tbp *TokenBlockProcessor) computeBlockHashes(chunks [][]uint32) []uint64 {
	hashes := make([]uint64, len(chunks))
	for i, chunk := range chunks {
		hashes[i] = computeStandardizedHash(chunk)
	}
	return hashes
}
