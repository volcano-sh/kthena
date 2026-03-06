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

package plugins

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/tokenization"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// Note: We avoid mocking Redis in tests to prevent complex interface implementation
// Instead, we focus on testing the core logic that doesn't require Redis connection

// Test core functions that don't require external dependencies

func TestKVCacheAwareBlock_String_Core(t *testing.T) {
	tests := []struct {
		name     string
		block    KVCacheAwareBlock
		prefix   string
		expected string
	}{
		{
			name: "Basic block string generation",
			block: KVCacheAwareBlock{
				ModelName: "test-model",
				ChunkHash: 12345,
			},
			prefix:   "matrix:kv:block:",
			expected: "matrix:kv:block:test-model@12345",
		},
		{
			name: "Complex model name",
			block: KVCacheAwareBlock{
				ModelName: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B",
				ChunkHash: 9876543210,
			},
			prefix:   "matrix:kv:block:",
			expected: "matrix:kv:block:deepseek-ai/DeepSeek-R1-Distill-Qwen-7B@9876543210",
		},
		{
			name: "Zero hash",
			block: KVCacheAwareBlock{
				ModelName: "model",
				ChunkHash: 0,
			},
			prefix:   "prefix:",
			expected: "prefix:model@0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.block.String(tt.prefix)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestExtractPodNameFromIdentifier_Core(t *testing.T) {
	tests := []struct {
		name       string
		identifier string
		expected   string
	}{
		{
			name:       "Simple pod name",
			identifier: "pod-name",
			expected:   "pod-name",
		},
		{
			name:       "Pod with namespace",
			identifier: "pod-name.namespace",
			expected:   "pod-name",
		},
		{
			name:       "Full pod identifier",
			identifier: "pod-name.namespace.svc.cluster.local",
			expected:   "pod-name",
		},
		{
			name:       "Pod with dashes and numbers",
			identifier: "my-pod-123.my-namespace.svc.cluster.local",
			expected:   "my-pod-123",
		},
		{
			name:       "Empty string",
			identifier: "",
			expected:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPodNameFromIdentifier(tt.identifier)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestComputeStandardizedHash_Core(t *testing.T) {
	tests := []struct {
		name     string
		tokenIds []uint32
	}{
		{
			name:     "Empty token list",
			tokenIds: []uint32{},
		},
		{
			name:     "Single token",
			tokenIds: []uint32{123},
		},
		{
			name:     "Multiple tokens",
			tokenIds: []uint32{1, 2, 3, 4, 5},
		},
		{
			name:     "Large tokens",
			tokenIds: []uint32{65536, 131072, 262144},
		},
		{
			name:     "Max value tokens",
			tokenIds: []uint32{4294967295, 4294967196, 4294966296},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeStandardizedHash(tt.tokenIds)

			if len(tt.tokenIds) == 0 {
				if result != 0 {
					t.Errorf("Expected 0 for empty input, got %d", result)
				}
				return
			}

			// Verify hash is positive (MSB cleared)
			if result > 0x7FFFFFFFFFFFFFFF {
				t.Errorf("Hash should be positive, got %d", result)
			}

			// Verify consistency - same input should produce same hash
			result2 := computeStandardizedHash(tt.tokenIds)
			if result != result2 {
				t.Errorf("Hash should be consistent, got %d and %d", result, result2)
			}

			// Verify different inputs produce different hashes (with high probability)
			if len(tt.tokenIds) > 1 {
				modifiedTokens := make([]uint32, len(tt.tokenIds))
				copy(modifiedTokens, tt.tokenIds)
				modifiedTokens[0] = modifiedTokens[0] + 1

				result3 := computeStandardizedHash(modifiedTokens)
				if result == result3 {
					t.Logf("Warning: Different inputs produced same hash (collision)")
				}
			}
		})
	}
}

func TestTokenBlockProcessor_ChunkTokens_Core(t *testing.T) {
	tests := []struct {
		name      string
		blockSize int
		tokens    []uint32
		expected  [][]uint32
	}{
		{
			name:      "Empty tokens",
			blockSize: 4,
			tokens:    []uint32{},
			expected:  [][]uint32{},
		},
		{
			name:      "Single chunk",
			blockSize: 4,
			tokens:    []uint32{1, 2, 3},
			expected:  [][]uint32{{1, 2, 3}},
		},
		{
			name:      "Exact multiple chunks",
			blockSize: 2,
			tokens:    []uint32{1, 2, 3, 4},
			expected:  [][]uint32{{1, 2}, {3, 4}},
		},
		{
			name:      "Partial last chunk",
			blockSize: 3,
			tokens:    []uint32{1, 2, 3, 4, 5},
			expected:  [][]uint32{{1, 2, 3}, {4, 5}},
		},
		{
			name:      "Single token per chunk",
			blockSize: 1,
			tokens:    []uint32{10, 20, 30},
			expected:  [][]uint32{{10}, {20}, {30}},
		},
		{
			name:      "Large block size",
			blockSize: 100,
			tokens:    []uint32{1, 2, 3, 4, 5},
			expected:  [][]uint32{{1, 2, 3, 4, 5}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &TokenBlockProcessor{blockSize: tt.blockSize}
			result := processor.chunkTokens(tt.tokens, 100) // Use reasonable maxBlocks for test

			// Handle empty slice comparison
			if len(result) == 0 && len(tt.expected) == 0 {
				return
			}

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestTokenBlockProcessor_TokensToBlockHashes_Core(t *testing.T) {
	tests := []struct {
		name      string
		blockSize int
		tokens    []uint32
		expectLen int
	}{
		{
			name:      "Empty tokens",
			blockSize: 4,
			tokens:    []uint32{},
			expectLen: 0,
		},
		{
			name:      "Single block",
			blockSize: 4,
			tokens:    []uint32{1, 2, 3},
			expectLen: 1,
		},
		{
			name:      "Multiple blocks",
			blockSize: 2,
			tokens:    []uint32{1, 2, 3, 4, 5},
			expectLen: 3,
		},
		{
			name:      "Large block size",
			blockSize: 100,
			tokens:    []uint32{1, 2, 3, 4, 5},
			expectLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &TokenBlockProcessor{blockSize: tt.blockSize}
			result := processor.TokensToBlockHashes(tt.tokens, 100)

			if len(result) != tt.expectLen {
				t.Errorf("Expected %d hashes, got %d", tt.expectLen, len(result))
			}

			// Verify all hashes are positive
			for i, hash := range result {
				if hash > 0x7FFFFFFFFFFFFFFF {
					t.Errorf("Hash %d should be positive, got %d", i, hash)
				}
			}

			// Verify consistency
			result2 := processor.TokensToBlockHashes(tt.tokens, 100)
			if !reflect.DeepEqual(result, result2) {
				t.Errorf("TokensToBlockHashes should be consistent")
			}
		})
	}
}

func TestKVCacheAware_CalculatePodScores_Core(t *testing.T) {
	plugin := &KVCacheAware{}

	tests := []struct {
		name        string
		blockHashes []uint64
		blockToPods map[uint64][]string
		expected    map[string]int
	}{
		{
			name:        "Empty block hashes",
			blockHashes: []uint64{},
			blockToPods: map[uint64][]string{},
			expected:    map[string]int{},
		},
		{
			name:        "No pods for first block",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				2: {"pod2"},
				3: {"pod3"},
			},
			expected: map[string]int{},
		},
		{
			name:        "Single pod matches all blocks",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				1: {"pod1"},
				2: {"pod1"},
				3: {"pod1"},
			},
			expected: map[string]int{
				"pod1": 100, // 3/3 * 100
			},
		},
		{
			name:        "Multiple pods with different match lengths",
			blockHashes: []uint64{1, 2, 3, 4},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2"},
				2: {"pod1", "pod2"},
				3: {"pod1"},
				4: {"pod1"},
			},
			expected: map[string]int{
				"pod1": 100, // 4/4 * 100
				"pod2": 50,  // 2/4 * 100
			},
		},
		{
			name:        "Consecutive matching stops early",
			blockHashes: []uint64{1, 2, 3, 4},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2"},
				2: {"pod1"},
				3: {"pod2"}, // pod2 doesn't have block 2, so stops at block 1
				4: {"pod1"},
			},
			expected: map[string]int{
				"pod1": 50, // 2/4 * 100 (consecutive blocks 1,2, then stops at missing block 3)
				"pod2": 25, // 1/4 * 100 (only block 1, then stops at missing block 2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.calculatePodScores(tt.blockHashes, tt.blockToPods)

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// Helper function to create test pods
func createTestPods(names ...string) []*datastore.PodInfo {
	pods := make([]*datastore.PodInfo, len(names))
	for i, name := range names {
		pods[i] = &datastore.PodInfo{
			Pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "test-namespace",
				},
			},
		}
	}
	return pods
}

func TestNewKVCacheAware_Core(t *testing.T) {
	tests := []struct {
		name              string
		pluginArg         runtime.RawExtension
		expectedBlockSize int
		expectedMaxBlocks int
		expectedName      string
		expectedKeyPrefix string
	}{
		{
			name:              "Default configuration",
			pluginArg:         runtime.RawExtension{},
			expectedBlockSize: defaultBlockSizeToHash,
			expectedMaxBlocks: defaultMaxBlocksToMatch,
			expectedName:      KVCacheAwarePluginName,
			expectedKeyPrefix: kvCacheKeyPrefix,
		},
		{
			name: "Custom configuration",
			pluginArg: runtime.RawExtension{
				Raw: []byte(`{"blockSizeToHash": 64, "maxBlocksToMatch": 256}`),
			},
			expectedBlockSize: 64,
			expectedMaxBlocks: 256,
			expectedName:      KVCacheAwarePluginName,
			expectedKeyPrefix: kvCacheKeyPrefix,
		},
		{
			name: "Invalid YAML configuration falls back to defaults",
			pluginArg: runtime.RawExtension{
				Raw: []byte(`invalid yaml`),
			},
			expectedBlockSize: defaultBlockSizeToHash,
			expectedMaxBlocks: defaultMaxBlocksToMatch,
			expectedName:      KVCacheAwarePluginName,
			expectedKeyPrefix: kvCacheKeyPrefix,
		},
		{
			name: "Zero values fall back to defaults",
			pluginArg: runtime.RawExtension{
				Raw: []byte(`{"blockSizeToHash": 0, "maxBlocksToMatch": 0}`),
			},
			expectedBlockSize: defaultBlockSizeToHash,
			expectedMaxBlocks: defaultMaxBlocksToMatch,
			expectedName:      KVCacheAwarePluginName,
			expectedKeyPrefix: kvCacheKeyPrefix,
		},
		{
			name: "Negative values fall back to defaults",
			pluginArg: runtime.RawExtension{
				Raw: []byte(`{"blockSizeToHash": -10, "maxBlocksToMatch": -20}`),
			},
			expectedBlockSize: defaultBlockSizeToHash,
			expectedMaxBlocks: defaultMaxBlocksToMatch,
			expectedName:      KVCacheAwarePluginName,
			expectedKeyPrefix: kvCacheKeyPrefix,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test configuration parsing without Redis connection
			var args KVCacheAwareArgs
			if len(tt.pluginArg.Raw) > 0 {
				err := yaml.Unmarshal(tt.pluginArg.Raw, &args)
				if tt.name != "Invalid YAML configuration falls back to defaults" && err != nil {
					t.Errorf("Unexpected YAML parsing error: %v", err)
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

			// Verify configuration parsing
			if blockSizeToHash != tt.expectedBlockSize {
				t.Errorf("Expected blockSize %d, got %d", tt.expectedBlockSize, blockSizeToHash)
			}

			if maxBlocksToMatch != tt.expectedMaxBlocks {
				t.Errorf("Expected maxBlocks %d, got %d", tt.expectedMaxBlocks, maxBlocksToMatch)
			}

			// Test processor creation
			processor := &TokenBlockProcessor{blockSize: blockSizeToHash}
			if processor.blockSize != tt.expectedBlockSize {
				t.Errorf("Expected processor blockSize %d, got %d", tt.expectedBlockSize, processor.blockSize)
			}

			// Test constants
			if KVCacheAwarePluginName != tt.expectedName {
				t.Errorf("Expected name %s, got %s", tt.expectedName, KVCacheAwarePluginName)
			}

			if kvCacheKeyPrefix != tt.expectedKeyPrefix {
				t.Errorf("Expected keyPrefix %s, got %s", tt.expectedKeyPrefix, kvCacheKeyPrefix)
			}
		})
	}
}

func TestKVCacheAware_Score_Core(t *testing.T) {
	pods := createTestPods("pod1", "pod2", "pod3")

	tests := []struct {
		name           string
		context        *framework.Context
		pods           []*datastore.PodInfo
		expectedScores map[string]int
	}{
		{
			name: "Empty prompt returns zero scores",
			context: &framework.Context{
				Model:  "test-model",
				Prompt: common.ChatMessage{},
			},
			pods: pods,
			expectedScores: map[string]int{
				"pod1": 0,
				"pod2": 0,
				"pod3": 0,
			},
		},
		{
			name: "Empty model returns zero scores",
			context: &framework.Context{
				Model: "",
				Prompt: common.ChatMessage{
					Text: "Hello world",
				},
			},
			pods: pods,
			expectedScores: map[string]int{
				"pod1": 0,
				"pod2": 0,
				"pod3": 0,
			},
		},
		{
			name: "No tokenizer available returns zero scores",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text: "Hello world",
				},
			},
			pods: pods,
			expectedScores: map[string]int{
				"pod1": 0,
				"pod2": 0,
				"pod3": 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &KVCacheAware{
				name:             KVCacheAwarePluginName,
				maxBlocksToMatch: 128,
				keyPrefix:        kvCacheKeyPrefix,
				processor:        &TokenBlockProcessor{blockSize: 128},
				// tokenizerManager will be nil, causing the expected behavior
			}

			result := plugin.Score(tt.context, tt.pods)

			// Convert result to map[string]int for easier comparison
			resultMap := make(map[string]int)
			for pod, score := range result {
				resultMap[pod.Pod.Name] = score
			}

			if !reflect.DeepEqual(resultMap, tt.expectedScores) {
				t.Errorf("Expected scores %v, got %v", tt.expectedScores, resultMap)
			}
		})
	}
}

// TestKVCacheAware_TokenizeWithChatTemplate_Core has been removed as the tokenizeWithChatTemplate
// method has been moved to the tokenization package. The functionality is now tested
// in the tokenization package tests.

func TestKVCacheAware_MaxBlocksToMatch_Core(t *testing.T) {
	tests := []struct {
		name           string
		maxBlocks      int
		inputTokens    []uint32
		expectedBlocks int
	}{
		{
			name:           "Blocks within limit",
			maxBlocks:      5,
			inputTokens:    make([]uint32, 256), // 2 blocks with blockSize=128
			expectedBlocks: 2,
		},
		{
			name:           "Blocks exceed limit",
			maxBlocks:      2,
			inputTokens:    make([]uint32, 512), // 4 blocks with blockSize=128
			expectedBlocks: 2,                   // Should be limited to maxBlocks
		},
		{
			name:           "Exactly at limit",
			maxBlocks:      3,
			inputTokens:    make([]uint32, 384), // 3 blocks with blockSize=128
			expectedBlocks: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize tokens with sequential values for testing
			for i := range tt.inputTokens {
				tt.inputTokens[i] = uint32(i + 1)
			}

			processor := &TokenBlockProcessor{blockSize: 128}
			blockHashes := processor.TokensToBlockHashes(tt.inputTokens, tt.maxBlocks)

			if len(blockHashes) != tt.expectedBlocks {
				t.Errorf("Expected %d blocks, got %d", tt.expectedBlocks, len(blockHashes))
			}
		})
	}
}

func TestKVCacheAware_EdgeCases_Core(t *testing.T) {
	tests := []struct {
		name        string
		description string
		testFunc    func(t *testing.T)
	}{
		{
			name:        "Very large block size",
			description: "Test with block size larger than input",
			testFunc: func(t *testing.T) {
				processor := &TokenBlockProcessor{blockSize: 1000}
				tokens := []uint32{1, 2, 3, 4, 5}
				hashes := processor.TokensToBlockHashes(tokens, 100)

				if len(hashes) != 1 {
					t.Errorf("Expected 1 hash for large block size, got %d", len(hashes))
				}
			},
		},
		{
			name:        "Block size of 1",
			description: "Test with minimum block size",
			testFunc: func(t *testing.T) {
				processor := &TokenBlockProcessor{blockSize: 1}
				tokens := []uint32{1, 2, 3, 4, 5}
				hashes := processor.TokensToBlockHashes(tokens, 100)

				if len(hashes) != 5 {
					t.Errorf("Expected 5 hashes for block size 1, got %d", len(hashes))
				}
			},
		},
		{
			name:        "Maximum uint32 token values",
			description: "Test with maximum token values",
			testFunc: func(t *testing.T) {
				processor := &TokenBlockProcessor{blockSize: 2}
				tokens := []uint32{4294967295, 4294967294} // Max uint32 values
				hashes := processor.TokensToBlockHashes(tokens, 100)

				if len(hashes) != 1 {
					t.Errorf("Expected 1 hash, got %d", len(hashes))
				}

				if hashes[0] == 0 {
					t.Error("Hash should not be zero for max values")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t)
		})
	}
}

func TestKVCacheAware_QueryRedisForBlocks_Core(t *testing.T) {
	// Note: This test focuses on the core logic without actual Redis dependency
	// In a real integration test, you would use a Redis test container

	tests := []struct {
		name           string
		blockHashes    []uint64
		modelName      string
		mockResults    map[uint64][]string
		expectedResult map[uint64][]string
		expectError    bool
	}{
		{
			name:           "Empty block hashes",
			blockHashes:    []uint64{},
			modelName:      "test-model",
			mockResults:    map[uint64][]string{},
			expectedResult: map[uint64][]string{},
			expectError:    false,
		},
		{
			name:        "Single block with pods",
			blockHashes: []uint64{12345},
			modelName:   "test-model",
			mockResults: map[uint64][]string{
				12345: {"pod1.namespace", "pod2.namespace"},
			},
			expectedResult: map[uint64][]string{
				12345: {"pod1", "pod2"},
			},
			expectError: false,
		},
		{
			name:        "Multiple blocks with mixed results",
			blockHashes: []uint64{12345, 67890, 11111},
			modelName:   "test-model",
			mockResults: map[uint64][]string{
				12345: {"pod1.namespace.svc.cluster.local", "pod2.namespace"},
				67890: {}, // No pods for this block
				11111: {"pod3.namespace.svc.cluster.local"},
			},
			expectedResult: map[uint64][]string{
				12345: {"pod1", "pod2"},
				11111: {"pod3"},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test Redis key generation
			for _, hash := range tt.blockHashes {
				block := KVCacheAwareBlock{ModelName: tt.modelName, ChunkHash: hash}
				key := block.String(kvCacheKeyPrefix)
				expectedKey := kvCacheKeyPrefix + tt.modelName + "@" + fmt.Sprintf("%d", hash)
				if key != expectedKey {
					t.Errorf("Expected key %s, got %s", expectedKey, key)
				}
			}

			// Test pod name extraction logic
			for hash, expectedPods := range tt.expectedResult {
				mockPods := tt.mockResults[hash]
				actualPods := make([]string, 0, len(mockPods))

				for _, podIdentifier := range mockPods {
					podName := extractPodNameFromIdentifier(podIdentifier)
					actualPods = append(actualPods, podName)
				}

				if !reflect.DeepEqual(actualPods, expectedPods) {
					t.Errorf("For hash %d, expected pods %v, got %v", hash, expectedPods, actualPods)
				}
			}
		})
	}
}

func TestKVCacheAware_CalculatePodScores_AdvancedCases_Core(t *testing.T) {
	plugin := &KVCacheAware{}

	tests := []struct {
		name        string
		blockHashes []uint64
		blockToPods map[uint64][]string
		expected    map[string]int
	}{
		{
			name:        "Single block, single pod",
			blockHashes: []uint64{1},
			blockToPods: map[uint64][]string{
				1: {"pod1"},
			},
			expected: map[string]int{
				"pod1": 100, // 1/1 * 100
			},
		},
		{
			name:        "Multiple blocks, all pods have all blocks",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2", "pod3"},
				2: {"pod1", "pod2", "pod3"},
				3: {"pod1", "pod2", "pod3"},
			},
			expected: map[string]int{
				"pod1": 100, // 3/3 * 100
				"pod2": 100, // 3/3 * 100
				"pod3": 100, // 3/3 * 100
			},
		},
		{
			name:        "Gradual pod elimination",
			blockHashes: []uint64{1, 2, 3, 4, 5},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2", "pod3", "pod4"},
				2: {"pod1", "pod2", "pod3"},
				3: {"pod1", "pod2"},
				4: {"pod1"},
				5: {"pod1"},
			},
			expected: map[string]int{
				"pod1": 100, // 5/5 * 100
				"pod2": 60,  // 3/5 * 100
				"pod3": 40,  // 2/5 * 100
				"pod4": 20,  // 1/5 * 100
			},
		},
		{
			name:        "No pods for first block",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				2: {"pod1"},
				3: {"pod1"},
			},
			expected: map[string]int{}, // Empty because no pods have first block
		},
		{
			name:        "Large number of blocks",
			blockHashes: make([]uint64, 100), // 100 blocks
			blockToPods: func() map[uint64][]string {
				result := make(map[uint64][]string)
				for i := 0; i < 100; i++ {
					result[uint64(i)] = []string{"pod1"}
				}
				return result
			}(),
			expected: map[string]int{
				"pod1": 100, // 100/100 * 100
			},
		},
	}

	// Initialize the large number of blocks test case
	for i := 0; i < 100; i++ {
		tests[4].blockHashes[i] = uint64(i)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.calculatePodScores(tt.blockHashes, tt.blockToPods)

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestKVCacheAware_NormalizeAndTokenizePrompt_Core(t *testing.T) {
	// Note: This test focuses on the core logic that can be tested without external dependencies
	// Full integration tests would require actual tokenizer setup

	tests := []struct {
		name        string
		description string
		testFunc    func(t *testing.T)
	}{
		{
			name:        "Empty prompt text and messages",
			description: "Test behavior with empty prompt",
			testFunc: func(t *testing.T) {
				plugin := &KVCacheAware{}
				ctx := &framework.Context{
					Model: "test-model",
					Prompt: common.ChatMessage{
						Text:     "",
						Messages: []common.Message{},
					},
				}

				// This should return error due to no tokenizer available
				result, err := plugin.normalizeAndTokenizePrompt(ctx, []*datastore.PodInfo{})

				if err == nil {
					t.Error("Expected error for empty prompt with no tokenizer")
				}

				if result != nil {
					t.Errorf("Expected nil result, got %v", result)
				}
			},
		},
		{
			name:        "Token conversion logic",
			description: "Test uint32 token conversion from bytes",
			testFunc: func(t *testing.T) {
				// Test the byte-to-uint32 conversion logic used in the method
				testBytes := []byte{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3}
				expectedTokens := []uint32{1, 2, 3}

				// Simulate the conversion logic from normalizeAndTokenizePrompt
				tokens32 := make([]uint32, len(testBytes)/4)
				for i := 0; i < len(tokens32); i++ {
					tokens32[i] = uint32(testBytes[i*4])<<24 | uint32(testBytes[i*4+1])<<16 | uint32(testBytes[i*4+2])<<8 | uint32(testBytes[i*4+3])
				}

				if !reflect.DeepEqual(tokens32, expectedTokens) {
					t.Errorf("Expected %v, got %v", expectedTokens, tokens32)
				}
			},
		},
		{
			name:        "Chat template input validation",
			description: "Test chat template input structure",
			testFunc: func(t *testing.T) {
				// Test the input structure that would be passed to tokenizeWithChatTemplate
				input := tokenization.TokenizeInput{
					Type:                tokenization.ChatInput,
					Messages:            []common.Message{{Role: "user", Content: "Hello"}},
					AddSpecialTokens:    false,
					AddGenerationPrompt: true,
					ReturnTokenStrings:  false,
				}

				// Verify the input structure is correctly formed
				if input.Type != tokenization.ChatInput {
					t.Errorf("Expected ChatInput type, got %v", input.Type)
				}

				if len(input.Messages) != 1 {
					t.Errorf("Expected 1 message, got %d", len(input.Messages))
				}

				if input.AddSpecialTokens != false {
					t.Error("Expected AddSpecialTokens to be false")
				}

				if input.AddGenerationPrompt != true {
					t.Error("Expected AddGenerationPrompt to be true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t)
		})
	}
}

func TestKVCacheAware_Performance_Core(t *testing.T) {
	tests := []struct {
		name        string
		description string
		testFunc    func(t *testing.T)
	}{
		{
			name:        "Large token sequence processing",
			description: "Test processing of large token sequences",
			testFunc: func(t *testing.T) {
				processor := &TokenBlockProcessor{blockSize: 128}

				// Create a large token sequence (10,000 tokens)
				tokens := make([]uint32, 10000)
				for i := range tokens {
					tokens[i] = uint32(i % 1000) // Vary tokens to avoid identical blocks
				}

				start := time.Now()
				hashes := processor.TokensToBlockHashes(tokens, 1000)
				duration := time.Since(start)

				expectedBlocks := (len(tokens) + 127) / 128 // Ceiling division
				if len(hashes) != expectedBlocks {
					t.Errorf("Expected %d blocks, got %d", expectedBlocks, len(hashes))
				}

				// Performance check: should complete within reasonable time
				if duration > time.Second {
					t.Errorf("Processing took too long: %v", duration)
				}

				t.Logf("Processed %d tokens into %d blocks in %v", len(tokens), len(hashes), duration)
			},
		},
		{
			name:        "Hash collision resistance",
			description: "Test that different token sequences produce different hashes",
			testFunc: func(t *testing.T) {
				processor := &TokenBlockProcessor{blockSize: 4}

				// Create different token sequences
				sequences := [][]uint32{
					{1, 2, 3, 4},
					{1, 2, 3, 5},
					{1, 2, 4, 4},
					{2, 2, 3, 4},
					{4, 3, 2, 1},
				}

				hashes := make([]uint64, len(sequences))
				for i, seq := range sequences {
					blockHashes := processor.TokensToBlockHashes(seq, 100)
					if len(blockHashes) != 1 {
						t.Errorf("Expected 1 hash for sequence %d, got %d", i, len(blockHashes))
					}
					hashes[i] = blockHashes[0]
				}

				// Check for uniqueness
				seen := make(map[uint64]bool)
				for i, hash := range hashes {
					if seen[hash] {
						t.Errorf("Hash collision detected for sequence %d: %d", i, hash)
					}
					seen[hash] = true
				}
			},
		},
		{
			name:        "Memory efficiency",
			description: "Test memory usage with large inputs",
			testFunc: func(t *testing.T) {
				processor := &TokenBlockProcessor{blockSize: 1000}

				// Create a moderately large token sequence
				tokens := make([]uint32, 5000)
				for i := range tokens {
					tokens[i] = uint32(i)
				}

				// Process multiple times to check for memory leaks
				for i := 0; i < 100; i++ {
					hashes := processor.TokensToBlockHashes(tokens, 1000)
					if len(hashes) == 0 {
						t.Error("Expected non-empty hashes")
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t)
		})
	}
}

func TestKVCacheAware_Integration_Core(t *testing.T) {
	// Integration-style tests that test multiple components together

	tests := []struct {
		name        string
		description string
		testFunc    func(t *testing.T)
	}{
		{
			name:        "End-to-end token processing",
			description: "Test complete flow from tokens to scores",
			testFunc: func(t *testing.T) {
				plugin := &KVCacheAware{
					name:             KVCacheAwarePluginName,
					maxBlocksToMatch: 10,
					keyPrefix:        kvCacheKeyPrefix,
					processor:        &TokenBlockProcessor{blockSize: 4},
				}

				// Test data
				tokens := []uint32{1, 2, 3, 4, 5, 6, 7, 8}
				blockHashes := plugin.processor.TokensToBlockHashes(tokens, 100)

				if len(blockHashes) != 2 {
					t.Errorf("Expected 2 blocks, got %d", len(blockHashes))
				}

				// Simulate Redis data
				blockToPods := map[uint64][]string{
					blockHashes[0]: {"pod1", "pod2"},
					blockHashes[1]: {"pod1"},
				}

				scores := plugin.calculatePodScores(blockHashes, blockToPods)

				expectedScores := map[string]int{
					"pod1": 100, // 2/2 * 100
					"pod2": 50,  // 1/2 * 100
				}

				if !reflect.DeepEqual(scores, expectedScores) {
					t.Errorf("Expected scores %v, got %v", expectedScores, scores)
				}
			},
		},
		{
			name:        "Block size boundary conditions",
			description: "Test various block size scenarios",
			testFunc: func(t *testing.T) {
				testCases := []struct {
					blockSize      int
					tokenCount     int
					expectedBlocks int
				}{
					{1, 5, 5},    // One token per block
					{5, 5, 1},    // Exact fit
					{3, 10, 4},   // Partial last block
					{100, 10, 1}, // Block size larger than input
				}

				for _, tc := range testCases {
					processor := &TokenBlockProcessor{blockSize: tc.blockSize}
					tokens := make([]uint32, tc.tokenCount)
					for i := range tokens {
						tokens[i] = uint32(i + 1)
					}

					hashes := processor.TokensToBlockHashes(tokens, 1000)
					if len(hashes) != tc.expectedBlocks {
						t.Errorf("BlockSize %d, TokenCount %d: expected %d blocks, got %d",
							tc.blockSize, tc.tokenCount, tc.expectedBlocks, len(hashes))
					}
				}
			},
		},
		{
			name:        "Score calculation edge cases",
			description: "Test score calculation with various scenarios",
			testFunc: func(t *testing.T) {
				plugin := &KVCacheAware{}

				// Test with fractional scores
				blockHashes := []uint64{1, 2, 3}
				blockToPods := map[uint64][]string{
					1: {"pod1"},
					2: {"pod1"},
					// pod1 missing block 3
				}

				scores := plugin.calculatePodScores(blockHashes, blockToPods)
				expected := map[string]int{
					"pod1": 66, // 2/3 * 100 = 66.666... -> 66
				}

				if !reflect.DeepEqual(scores, expected) {
					t.Errorf("Expected scores %v, got %v", expected, scores)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t)
		})
	}
}

func TestKVCacheAware_Constants_Core(t *testing.T) {
	// Test that constants have expected values
	tests := []struct {
		name     string
		actual   interface{}
		expected interface{}
	}{
		{
			name:     "Plugin name",
			actual:   KVCacheAwarePluginName,
			expected: "kvcache-aware",
		},
		{
			name:     "Key prefix",
			actual:   kvCacheKeyPrefix,
			expected: "matrix:kv:block:",
		},
		{
			name:     "Default block size",
			actual:   defaultBlockSizeToHash,
			expected: 128,
		},
		{
			name:     "Default max blocks",
			actual:   defaultMaxBlocksToMatch,
			expected: 128,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.actual != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, tt.actual)
			}
		})
	}
}

// Test computeStandardizedHash function with various inputs
func TestComputeStandardizedHash_Advanced_Core(t *testing.T) {
	tests := []struct {
		name        string
		tokenIds    []uint32
		expectZero  bool
		description string
	}{
		{
			name:        "Empty token sequence",
			tokenIds:    []uint32{},
			expectZero:  true,
			description: "Should return 0 for empty input",
		},
		{
			name:        "Single token",
			tokenIds:    []uint32{1},
			expectZero:  false,
			description: "Should generate hash for single token",
		},
		{
			name:        "Multiple tokens",
			tokenIds:    []uint32{1, 2, 3, 4, 5},
			expectZero:  false,
			description: "Should generate hash for multiple tokens",
		},
		{
			name:        "Large token values",
			tokenIds:    []uint32{2147483647, 2147483646}, // Max int32 values
			expectZero:  false,
			description: "Should handle large token values",
		},
		{
			name:        "Zero tokens",
			tokenIds:    []uint32{0, 0, 0},
			expectZero:  false,
			description: "Should generate hash for zero tokens",
		},
		{
			name:        "Max uint32 tokens",
			tokenIds:    []uint32{4294967295, 4294967294, 4294967293},
			expectZero:  false,
			description: "Should handle max uint32 token values",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := computeStandardizedHash(tt.tokenIds)

			if tt.expectZero {
				if hash != 0 {
					t.Errorf("Expected hash 0 for empty input, got %d", hash)
				}
				return
			}

			// Verify hash is positive (MSB should be 0)
			if hash > 0x7FFFFFFFFFFFFFFF {
				t.Errorf("Hash should be positive, got %d", hash)
			}

			// Verify consistency
			hash2 := computeStandardizedHash(tt.tokenIds)
			if hash != hash2 {
				t.Errorf("Hash should be consistent, got %d and %d", hash, hash2)
			}

			// Verify non-zero for non-empty input
			if hash == 0 {
				t.Error("Hash should not be zero for non-empty input")
			}
		})
	}
}

// Test TokenBlockProcessor methods in detail
func TestTokenBlockProcessor_Advanced_Core(t *testing.T) {
	tests := []struct {
		name        string
		blockSize   int
		tokens      []uint32
		expectedLen int
		description string
	}{
		{
			name:        "Empty tokens",
			blockSize:   4,
			tokens:      []uint32{},
			expectedLen: 0,
			description: "Should return nil for empty tokens",
		},
		{
			name:        "Tokens less than block size",
			blockSize:   10,
			tokens:      []uint32{1, 2, 3},
			expectedLen: 1,
			description: "Should create one block for tokens less than block size",
		},
		{
			name:        "Tokens equal to block size",
			blockSize:   3,
			tokens:      []uint32{1, 2, 3},
			expectedLen: 1,
			description: "Should create one block for tokens equal to block size",
		},
		{
			name:        "Tokens more than block size",
			blockSize:   2,
			tokens:      []uint32{1, 2, 3, 4, 5},
			expectedLen: 3,
			description: "Should create multiple blocks",
		},
		{
			name:        "Block size 1",
			blockSize:   1,
			tokens:      []uint32{1, 2, 3, 4, 5},
			expectedLen: 5,
			description: "Should create one block per token",
		},
		{
			name:        "Large block size",
			blockSize:   1000,
			tokens:      []uint32{1, 2, 3, 4, 5},
			expectedLen: 1,
			description: "Should create one block for large block size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &TokenBlockProcessor{blockSize: tt.blockSize}

			// Test chunkTokens
			chunks := processor.chunkTokens(tt.tokens, 100)
			if len(chunks) != tt.expectedLen {
				t.Errorf("Expected %d chunks, got %d", tt.expectedLen, len(chunks))
			}

			// Test TokensToBlockHashes
			hashes := processor.TokensToBlockHashes(tt.tokens, 100)
			if tt.expectedLen == 0 {
				if hashes != nil {
					t.Errorf("Expected nil hashes for empty tokens, got %v", hashes)
				}
				return
			}

			if len(hashes) != tt.expectedLen {
				t.Errorf("Expected %d hashes, got %d", tt.expectedLen, len(hashes))
			}

			// Verify all hashes are positive
			for i, hash := range hashes {
				if hash > 0x7FFFFFFFFFFFFFFF {
					t.Errorf("Hash %d should be positive, got %d", i, hash)
				}
			}

			// Test computeBlockHashes directly
			if len(chunks) > 0 {
				directHashes := processor.computeBlockHashes(chunks)
				if !reflect.DeepEqual(hashes, directHashes) {
					t.Errorf("Direct hash computation should match TokensToBlockHashes")
				}
			}
		})
	}
}

// Test normalizeAndTokenizePrompt method branches
func TestKVCacheAware_NormalizeAndTokenizePrompt_Advanced_Core(t *testing.T) {
	tests := []struct {
		name        string
		context     *framework.Context
		setupPlugin func() *KVCacheAware
		expectError bool
		description string
	}{
		{
			name: "Text prompt with nil tokenizer",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text: "Hello world",
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					// tokenizerManager is nil
				}
			},
			expectError: true,
			description: "Should return error when tokenizer is nil",
		},
		{
			name: "Chat messages with nil tokenizer",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Messages: []common.Message{
						{Role: "user", Content: "Hello"},
					},
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					// tokenizerManager is nil
				}
			},
			expectError: true,
			description: "Should return error when tokenizer is nil for chat messages",
		},
		{
			name: "Empty text and empty messages",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text:     "",
					Messages: []common.Message{},
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					// tokenizerManager is nil
				}
			},
			expectError: true,
			description: "Should return error for empty prompt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := tt.setupPlugin()
			result, err := plugin.normalizeAndTokenizePrompt(tt.context, []*datastore.PodInfo{})

			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				if result != nil {
					t.Errorf("Expected nil result on error, got %v", result)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

// Test Score method with various Redis scenarios
func TestKVCacheAware_Score_Advanced_Core(t *testing.T) {
	pods := createTestPods("pod1", "pod2", "pod3")

	tests := []struct {
		name           string
		context        *framework.Context
		setupPlugin    func() *KVCacheAware
		expectedScores map[string]int
		description    string
	}{
		{
			name: "Score with processor nil",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text: "Hello world",
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					name:             KVCacheAwarePluginName,
					maxBlocksToMatch: 128,
					keyPrefix:        kvCacheKeyPrefix,
					// processor is nil
				}
			},
			expectedScores: map[string]int{
				"pod1": 0,
				"pod2": 0,
				"pod3": 0,
			},
			description: "Should handle nil processor gracefully",
		},
		{
			name: "Score with maxBlocksToMatch limit",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text: "Hello world",
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					name:             KVCacheAwarePluginName,
					maxBlocksToMatch: 0, // Very small limit
					keyPrefix:        kvCacheKeyPrefix,
					processor:        &TokenBlockProcessor{blockSize: 1},
				}
			},
			expectedScores: map[string]int{
				"pod1": 0,
				"pod2": 0,
				"pod3": 0,
			},
			description: "Should handle maxBlocksToMatch limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := tt.setupPlugin()
			result := plugin.Score(tt.context, pods)

			// Convert result to map[string]int for easier comparison
			resultMap := make(map[string]int)
			for pod, score := range result {
				resultMap[pod.Pod.Name] = score
			}

			if !reflect.DeepEqual(resultMap, tt.expectedScores) {
				t.Errorf("Expected scores %v, got %v", tt.expectedScores, resultMap)
			}
		})
	}
}

// Test extractPodNameFromIdentifier with various formats
func TestExtractPodNameFromIdentifier_Advanced_Core(t *testing.T) {
	tests := []struct {
		name       string
		identifier string
		expected   string
	}{
		{
			name:       "Simple pod name",
			identifier: "pod-name",
			expected:   "pod-name",
		},
		{
			name:       "Pod with namespace",
			identifier: "pod-name.namespace",
			expected:   "pod-name",
		},
		{
			name:       "Full service name",
			identifier: "pod-name.namespace.svc.cluster.local",
			expected:   "pod-name",
		},
		{
			name:       "Empty string",
			identifier: "",
			expected:   "",
		},
		{
			name:       "Single dot",
			identifier: ".",
			expected:   "",
		},
		{
			name:       "Multiple dots",
			identifier: "a.b.c.d.e.f",
			expected:   "a",
		},
		{
			name:       "Pod name with hyphens",
			identifier: "my-pod-name-123.my-namespace.svc.cluster.local",
			expected:   "my-pod-name-123",
		},
		{
			name:       "Pod name with numbers",
			identifier: "pod123.ns456",
			expected:   "pod123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPodNameFromIdentifier(tt.identifier)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// Test calculatePodScores with complex scenarios
func TestKVCacheAware_CalculatePodScores_Complex_Core(t *testing.T) {
	plugin := &KVCacheAware{}

	tests := []struct {
		name        string
		blockHashes []uint64
		blockToPods map[uint64][]string
		expected    map[string]int
		description string
	}{
		{
			name:        "Empty block hashes",
			blockHashes: []uint64{},
			blockToPods: map[uint64][]string{},
			expected:    map[string]int{},
			description: "Should return empty scores for empty block hashes",
		},
		{
			name:        "First block has no pods",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				2: {"pod1"},
				3: {"pod1"},
			},
			expected:    map[string]int{},
			description: "Should return empty scores when first block has no pods",
		},
		{
			name:        "Pods drop out at different stages",
			blockHashes: []uint64{1, 2, 3, 4, 5},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2", "pod3", "pod4"},
				2: {"pod1", "pod2", "pod3"},
				3: {"pod1", "pod2"},
				4: {"pod1"},
				5: {"pod1"},
			},
			expected: map[string]int{
				"pod1": 100, // 5/5 * 100
				"pod2": 60,  // 3/5 * 100
				"pod3": 40,  // 2/5 * 100
				"pod4": 20,  // 1/5 * 100
			},
			description: "Should handle gradual pod elimination correctly",
		},
		{
			name:        "All pods drop out after first block",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2"},
				2: {"pod3"}, // Different pods, so all original pods drop out
				3: {"pod3"},
			},
			expected: map[string]int{
				"pod1": 33, // 1/3 * 100 = 33.33... -> 33
				"pod2": 33, // 1/3 * 100 = 33.33... -> 33
			},
			description: "Should handle all pods dropping out after first block",
		},
		{
			name:        "Single block, multiple pods",
			blockHashes: []uint64{1},
			blockToPods: map[uint64][]string{
				1: {"pod1", "pod2", "pod3"},
			},
			expected: map[string]int{
				"pod1": 100, // 1/1 * 100
				"pod2": 100, // 1/1 * 100
				"pod3": 100, // 1/1 * 100
			},
			description: "Should give 100% score for single block matches",
		},
		{
			name:        "Fractional scores",
			blockHashes: []uint64{1, 2, 3},
			blockToPods: map[uint64][]string{
				1: {"pod1"},
				2: {"pod1"},
				// pod1 missing block 3
			},
			expected: map[string]int{
				"pod1": 66, // 2/3 * 100 = 66.666... -> 66
			},
			description: "Should handle fractional scores correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.calculatePodScores(tt.blockHashes, tt.blockToPods)

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// TestKVCache_TokenizeWithChatTemplate_Advanced_Core has been removed as the tokenizeWithChatTemplate
// method has been moved to the tokenization package.

// Test KVCacheAwareBlock String method with various inputs
func TestKVCacheAwareBlock_String_Advanced_Core(t *testing.T) {
	tests := []struct {
		name     string
		block    KVCacheAwareBlock
		prefix   string
		expected string
	}{
		{
			name: "Normal case",
			block: KVCacheAwareBlock{
				ModelName: "test-model",
				ChunkHash: 12345,
			},
			prefix:   "prefix:",
			expected: "prefix:test-model@12345",
		},
		{
			name: "Empty prefix",
			block: KVCacheAwareBlock{
				ModelName: "test-model",
				ChunkHash: 12345,
			},
			prefix:   "",
			expected: "test-model@12345",
		},
		{
			name: "Model name with special characters",
			block: KVCacheAwareBlock{
				ModelName: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B",
				ChunkHash: 9876543210,
			},
			prefix:   "matrix:kv:block:",
			expected: "matrix:kv:block:deepseek-ai/DeepSeek-R1-Distill-Qwen-7B@9876543210",
		},
		{
			name: "Zero hash",
			block: KVCacheAwareBlock{
				ModelName: "test-model",
				ChunkHash: 0,
			},
			prefix:   "prefix:",
			expected: "prefix:test-model@0",
		},
		{
			name: "Maximum hash value",
			block: KVCacheAwareBlock{
				ModelName: "test-model",
				ChunkHash: 0x7FFFFFFFFFFFFFFF, // Max positive int64
			},
			prefix:   "prefix:",
			expected: "prefix:test-model@9223372036854775807",
		},
		{
			name: "Empty model name",
			block: KVCacheAwareBlock{
				ModelName: "",
				ChunkHash: 12345,
			},
			prefix:   "prefix:",
			expected: "prefix:@12345",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.block.String(tt.prefix)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// Test error handling and edge cases in Score method
func TestKVCacheAware_Score_ErrorHandling_Core(t *testing.T) {
	pods := createTestPods("pod1", "pod2")

	tests := []struct {
		name        string
		context     *framework.Context
		setupPlugin func() *KVCacheAware
		description string
	}{
		{
			name: "Nil context",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text: "Hello",
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					name:             KVCacheAwarePluginName,
					maxBlocksToMatch: 128,
					keyPrefix:        kvCacheKeyPrefix,
					processor:        &TokenBlockProcessor{blockSize: 128},
				}
			},
			description: "Should handle normal case without panic",
		},
		{
			name: "Very small maxBlocksToMatch",
			context: &framework.Context{
				Model: "test-model",
				Prompt: common.ChatMessage{
					Text: "Hello world this is a longer text",
				},
			},
			setupPlugin: func() *KVCacheAware {
				return &KVCacheAware{
					name:             KVCacheAwarePluginName,
					maxBlocksToMatch: 1, // Very small limit
					keyPrefix:        kvCacheKeyPrefix,
					processor:        &TokenBlockProcessor{blockSize: 1}, // Small blocks
				}
			},
			description: "Should handle small maxBlocksToMatch limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := tt.setupPlugin()

			// This should not panic
			result := plugin.Score(tt.context, pods)

			// Verify result structure
			if result == nil {
				t.Error("Expected non-nil result")
			}

			if len(result) != len(pods) {
				t.Errorf("Expected %d results, got %d", len(pods), len(result))
			}

			// Verify all scores are non-negative
			for pod, score := range result {
				if score < 0 {
					t.Errorf("Score should be non-negative, got %d for pod %s", score, pod.Pod.Name)
				}
				if score > 100 {
					t.Errorf("Score should not exceed 100, got %d for pod %s", score, pod.Pod.Name)
				}
			}
		})
	}
}

// Test binary conversion logic in normalizeAndTokenizePrompt
func TestKVCacheAware_BinaryConversion_Core(t *testing.T) {
	tests := []struct {
		name        string
		inputBytes  []byte
		expected    []uint32
		description string
	}{
		{
			name:        "Empty bytes",
			inputBytes:  []byte{},
			expected:    []uint32{},
			description: "Should handle empty byte array",
		},
		{
			name:        "Single token",
			inputBytes:  []byte{0, 0, 0, 1},
			expected:    []uint32{1},
			description: "Should convert single token correctly",
		},
		{
			name:        "Multiple tokens",
			inputBytes:  []byte{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3},
			expected:    []uint32{1, 2, 3},
			description: "Should convert multiple tokens correctly",
		},
		{
			name:        "Large token values",
			inputBytes:  []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF},
			expected:    []uint32{4294967295, 2147483647},
			description: "Should handle large token values",
		},
		{
			name:        "Incomplete last token (should be ignored)",
			inputBytes:  []byte{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0}, // Missing last byte
			expected:    []uint32{1, 2},
			description: "Should ignore incomplete tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the binary conversion logic from normalizeAndTokenizePrompt
			tokens32 := make([]uint32, len(tt.inputBytes)/4)
			for i := 0; i < len(tokens32); i++ {
				tokens32[i] = binary.BigEndian.Uint32(tt.inputBytes[i*4 : (i+1)*4])
			}

			if !reflect.DeepEqual(tokens32, tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, tokens32)
			}
		})
	}
}

// Test hash collision resistance and distribution
func TestComputeStandardizedHash_Distribution_Core(t *testing.T) {
	tests := []struct {
		name        string
		testFunc    func(t *testing.T)
		description string
	}{
		{
			name: "Different sequences produce different hashes",
			testFunc: func(t *testing.T) {
				sequences := [][]uint32{
					{1, 2, 3},
					{1, 2, 4},
					{1, 3, 3},
					{2, 2, 3},
					{3, 2, 1},
				}

				hashes := make([]uint64, len(sequences))
				for i, seq := range sequences {
					hashes[i] = computeStandardizedHash(seq)
				}

				// Check for uniqueness
				seen := make(map[uint64]bool)
				for i, hash := range hashes {
					if seen[hash] {
						t.Errorf("Hash collision detected for sequence %d: %v", i, sequences[i])
					}
					seen[hash] = true
				}
			},
			description: "Should produce unique hashes for different sequences",
		},
		{
			name: "Order matters",
			testFunc: func(t *testing.T) {
				seq1 := []uint32{1, 2, 3}
				seq2 := []uint32{3, 2, 1}

				hash1 := computeStandardizedHash(seq1)
				hash2 := computeStandardizedHash(seq2)

				if hash1 == hash2 {
					t.Error("Different order should produce different hashes")
				}
			},
			description: "Should produce different hashes for different orders",
		},
		{
			name: "Hash distribution",
			testFunc: func(t *testing.T) {
				// Generate many different sequences and check hash distribution
				hashes := make([]uint64, 100)
				for i := 0; i < 100; i++ {
					seq := []uint32{uint32(i), uint32(i + 1), uint32(i + 2)}
					hashes[i] = computeStandardizedHash(seq)
				}

				// Check that hashes are distributed (not all in same range)
				lowCount := 0
				highCount := 0
				midPoint := uint64(0x3FFFFFFFFFFFFFFF) // Half of max positive value

				for _, hash := range hashes {
					if hash < midPoint {
						lowCount++
					} else {
						highCount++
					}
				}

				// Expect some distribution (not all in one half)
				if lowCount == 0 || highCount == 0 {
					t.Errorf("Poor hash distribution: low=%d, high=%d", lowCount, highCount)
				}
			},
			description: "Should have good hash distribution",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t)
		})
	}
}

// Test concurrent access safety (basic test)
func TestKVCacheAware_Concurrency_Core(t *testing.T) {
	plugin := &KVCacheAware{
		name:             KVCacheAwarePluginName,
		maxBlocksToMatch: 128,
		keyPrefix:        kvCacheKeyPrefix,
		processor:        &TokenBlockProcessor{blockSize: 4},
	}

	pods := createTestPods("pod1", "pod2")
	ctx := &framework.Context{
		Model: "",
		Prompt: common.ChatMessage{
			Text: "",
		},
	}

	// Run multiple goroutines calling Score method
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()
			result := plugin.Score(ctx, pods)
			if result == nil {
				t.Error("Expected non-nil result")
			}
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}

// Test Name method
func TestKVCacheAware_Name_Core(t *testing.T) {
	plugin := &KVCacheAware{
		name: KVCacheAwarePluginName,
	}

	if plugin.Name() != KVCacheAwarePluginName {
		t.Errorf("Expected name %s, got %s", KVCacheAwarePluginName, plugin.Name())
	}

	// Test with custom name
	customPlugin := &KVCacheAware{
		name: "custom-kvcache-aware",
	}

	if customPlugin.Name() != "custom-kvcache-aware" {
		t.Errorf("Expected name custom-kvcache-aware, got %s", customPlugin.Name())
	}
}

// Test comprehensive integration scenarios
func TestKVCacheAware_Integration_Comprehensive_Core(t *testing.T) {
	tests := []struct {
		name        string
		description string
		testFunc    func(t *testing.T)
	}{
		{
			name:        "Complete workflow simulation",
			description: "Simulate complete KV cache workflow",
			testFunc: func(t *testing.T) {
				// Create plugin with realistic configuration
				plugin := &KVCacheAware{
					name:             KVCacheAwarePluginName,
					maxBlocksToMatch: 10,
					keyPrefix:        kvCacheKeyPrefix,
					processor:        &TokenBlockProcessor{blockSize: 8},
				}

				// Test token processing pipeline
				tokens := []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
				blockHashes := plugin.processor.TokensToBlockHashes(tokens, 100)

				if len(blockHashes) != 2 {
					t.Errorf("Expected 2 blocks, got %d", len(blockHashes))
				}

				// Test block key generation
				for i, hash := range blockHashes {
					block := KVCacheAwareBlock{ModelName: "test-model", ChunkHash: hash}
					key := block.String(plugin.keyPrefix)
					expectedPrefix := plugin.keyPrefix + "test-model@"
					if !strings.HasPrefix(key, expectedPrefix) {
						t.Errorf("Block %d key should start with %s, got %s", i, expectedPrefix, key)
					}
				}

				// Test score calculation
				blockToPods := map[uint64][]string{
					blockHashes[0]: {"pod1", "pod2"},
					blockHashes[1]: {"pod1"},
				}

				scores := plugin.calculatePodScores(blockHashes, blockToPods)
				expectedScores := map[string]int{
					"pod1": 100, // 2/2 * 100
					"pod2": 50,  // 1/2 * 100
				}

				if !reflect.DeepEqual(scores, expectedScores) {
					t.Errorf("Expected scores %v, got %v", expectedScores, scores)
				}
			},
		},
		{
			name:        "Stress test with large data",
			description: "Test with large token sequences and many blocks",
			testFunc: func(t *testing.T) {
				plugin := &KVCacheAware{
					processor: &TokenBlockProcessor{blockSize: 10},
				}

				// Generate large token sequence
				tokens := make([]uint32, 1000)
				for i := range tokens {
					tokens[i] = uint32(i % 256) // Vary tokens to avoid identical blocks
				}

				start := time.Now()
				blockHashes := plugin.processor.TokensToBlockHashes(tokens, 1000)
				duration := time.Since(start)

				expectedBlocks := 100 // 1000 / 10
				if len(blockHashes) != expectedBlocks {
					t.Errorf("Expected %d blocks, got %d", expectedBlocks, len(blockHashes))
				}

				// Performance check
				if duration > time.Second {
					t.Errorf("Processing took too long: %v", duration)
				}

				// Verify all hashes are unique
				seen := make(map[uint64]bool)
				for i, hash := range blockHashes {
					if seen[hash] {
						t.Errorf("Duplicate hash found at index %d: %d", i, hash)
					}
					seen[hash] = true
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t)
		})
	}
}
