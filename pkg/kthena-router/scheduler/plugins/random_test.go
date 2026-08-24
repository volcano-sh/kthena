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
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

func TestRandom_Name(t *testing.T) {
	plugin := NewRandom(runtime.RawExtension{})
	expected := RandomPluginName
	if plugin.Name() != expected {
		t.Errorf("Expected plugin name %s, got %s", expected, plugin.Name())
	}
}

func TestRandom_Score(t *testing.T) {
	plugin := NewRandom(runtime.RawExtension{})

	// Test with empty pods
	ctx := &framework.Context{}
	emptyPods := []*datastore.PodInfo{}
	scores := plugin.Score(ctx, emptyPods)
	if len(scores) != 0 {
		t.Errorf("Expected empty scores for empty pods, got %d scores", len(scores))
	}

	// Test with multiple pods
	pods := []*datastore.PodInfo{
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}}},
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}}},
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}}},
	}

	scores = plugin.Score(ctx, pods)

	// Check that all pods have scores
	if len(scores) != len(pods) {
		t.Errorf("Expected %d scores, got %d", len(pods), len(scores))
	}

	// Check that scores are within valid range [0, 100]
	for pod, score := range scores {
		if score < 0 || score > 100 {
			t.Errorf("Score for pod %s is out of range [0, 100]: %d", pod.Pod.Name, score)
		}
	}

	// Test multiple runs to ensure randomness (scores should be different)
	scores1 := plugin.Score(ctx, pods)
	scores2 := plugin.Score(ctx, pods)

	// While it's possible for scores to be the same by chance,
	// with 3 pods and 101 possible values, it's very unlikely
	// This is a basic sanity check for randomness
	allSame := true
	for _, pod := range pods {
		if scores1[pod] != scores2[pod] {
			allSame = false
			break
		}
	}

	// Note: This test might occasionally fail due to random chance,
	// but the probability is very low
	if allSame {
		t.Log("Warning: All scores were identical across two runs, which is unlikely but possible")
	}
}

// TestRandom_ConcurrentScore exercises Score from multiple goroutines, the way
// concurrent requests reach the single plugin instance the router builds once.
func TestRandom_ConcurrentScore(t *testing.T) {
	plugin := NewRandom(runtime.RawExtension{})
	ctx := &framework.Context{}
	pods := []*datastore.PodInfo{
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}}},
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}}},
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				scores := plugin.Score(ctx, pods)
				if len(scores) != len(pods) {
					t.Errorf("expected %d scores, got %d", len(pods), len(scores))
					return
				}
			}
		}()
	}
	wg.Wait()
}
