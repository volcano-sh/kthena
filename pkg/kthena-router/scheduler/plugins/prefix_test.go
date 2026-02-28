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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cespare/xxhash"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/cache"
)

func TestHashPrompt(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		prompt         string
		blockSize      int
		maxBlocks      int
		expectedHashes []uint64
	}{
		{
			name:           "Empty prompt",
			model:          "test-model",
			prompt:         "",
			blockSize:      64,
			maxBlocks:      128,
			expectedHashes: []uint64{},
		},
		{
			name:      "Single block prompt",
			model:     "test-model",
			prompt:    "Hello World",
			blockSize: 64,
			maxBlocks: 128,
			expectedHashes: []uint64{
				xxhash.Sum64([]byte(fmt.Sprintf("%dHello World", xxhash.Sum64([]byte("test-model"))))),
			},
		},
		{
			name:      "Multi block prompt",
			model:     "test-model",
			prompt:    "This is a longer prompt that should span multiple blocks based on the block size",
			blockSize: 20,
			maxBlocks: 128,
			expectedHashes: []uint64{
				xxhash.Sum64([]byte(fmt.Sprintf("%dThis is a longer pro", xxhash.Sum64([]byte("test-model"))))),
				xxhash.Sum64([]byte(fmt.Sprintf("%dmpt that should span", xxhash.Sum64([]byte(fmt.Sprintf("%dThis is a longer pro", xxhash.Sum64([]byte("test-model")))))))),
				xxhash.Sum64([]byte(fmt.Sprintf("%d multiple blocks bas", xxhash.Sum64([]byte(fmt.Sprintf("%dmpt that should span", xxhash.Sum64([]byte(fmt.Sprintf("%dThis is a longer pro", xxhash.Sum64([]byte("test-model"))))))))))),
				xxhash.Sum64([]byte(fmt.Sprintf("%ded on the block size", xxhash.Sum64([]byte(fmt.Sprintf("%d multiple blocks bas", xxhash.Sum64([]byte(fmt.Sprintf("%dmpt that should span", xxhash.Sum64([]byte(fmt.Sprintf("%dThis is a longer pro", xxhash.Sum64([]byte("test-model")))))))))))))),
			},
		},
		{
			name:      "Max blocks limit",
			model:     "test-model",
			prompt:    "A very long prompt " + strings.Repeat("test ", 100),
			blockSize: 10,
			maxBlocks: 3,
			expectedHashes: []uint64{
				xxhash.Sum64([]byte(fmt.Sprintf("%dA very lon", xxhash.Sum64([]byte("test-model"))))),
				xxhash.Sum64([]byte(fmt.Sprintf("%dg prompt t", xxhash.Sum64([]byte(fmt.Sprintf("%dA very lon", xxhash.Sum64([]byte("test-model")))))))),
				xxhash.Sum64([]byte(fmt.Sprintf("%dest test t", xxhash.Sum64([]byte(fmt.Sprintf("%dg prompt t", xxhash.Sum64([]byte(fmt.Sprintf("%dA very lon", xxhash.Sum64([]byte("test-model"))))))))))),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PrefixCache{
				blockSizeToHash:  tt.blockSize,
				maxBlocksToMatch: tt.maxBlocks,
			}
			got := p.hashPrompt(tt.model, tt.prompt)

			if !reflect.DeepEqual(got, tt.expectedHashes) {
				t.Errorf("hashPrompt() = %v, want %v", got, tt.expectedHashes)
			}
		})
	}
}

func TestPrefixCacheScore(t *testing.T) {
	// We construct a minimal PrefixCache by hand to avoid yaml/flag plumbing.
	t.Run("all pods present in score map, non-matching pods score 0", func(t *testing.T) {
		mockDS := datastore.New()
		prefixStore := cache.NewModelPrefixStore(mockDS, 100, 5)

		plugin := &PrefixCache{
			name:             PrefixCachePluginName,
			blockSizeToHash:  64,
			maxBlocksToMatch: 128,
			store:            prefixStore,
		}

		pod1 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}}
		pod2 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"}}}
		pod3 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Namespace: "ns1"}}}

		// Pre-populate cache: only pod1 has a matching prefix for "hello world"
		prompt := "hello world"
		hashes := plugin.hashPrompt("test-model", prompt)
		prefixStore.Add("test-model", hashes, pod1)

		ctx := &framework.Context{
			Model:  "test-model",
			Prompt: common.ChatMessage{Text: prompt},
		}
		scores := plugin.Score(ctx, []*datastore.PodInfo{pod1, pod2, pod3})

		// All three pods must be present in the map.
		if _, ok := scores[pod1]; !ok {
			t.Errorf("pod1 missing from score map")
		}
		if _, ok := scores[pod2]; !ok {
			t.Errorf("pod2 missing from score map")
		}
		if _, ok := scores[pod3]; !ok {
			t.Errorf("pod3 missing from score map")
		}

		// pod1 should have a non-zero score (full match).
		if scores[pod1] <= 0 {
			t.Errorf("pod1 score should be > 0, got %d", scores[pod1])
		}
		// pod2 and pod3 were never added to the cache – score must be 0.
		if scores[pod2] != 0 {
			t.Errorf("pod2 score should be 0, got %d", scores[pod2])
		}
		if scores[pod3] != 0 {
			t.Errorf("pod3 score should be 0, got %d", scores[pod3])
		}
	})

	t.Run("empty prompt returns nil", func(t *testing.T) {
		mockDS := datastore.New()
		prefixStore := cache.NewModelPrefixStore(mockDS, 100, 5)
		plugin := &PrefixCache{
			name:             PrefixCachePluginName,
			blockSizeToHash:  64,
			maxBlocksToMatch: 128,
			store:            prefixStore,
		}
		pod := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}}
		ctx := &framework.Context{
			Model:  "test-model",
			Prompt: common.ChatMessage{}, // empty – no Text, no Messages
		}
		scores := plugin.Score(ctx, []*datastore.PodInfo{pod})
		if scores != nil {
			t.Errorf("expected nil for empty prompt, got %v", scores)
		}
	})
}
