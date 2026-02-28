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

package cache

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestModelPrefixStore(t *testing.T) {
	tests := []struct {
		name         string
		maxHashes    int
		topK         int
		model        string
		pods         []*datastore.PodInfo
		addHashes    [][]uint64 // hashes to add for each pod
		queryHashes  []uint64   // hashes to query
		expectedPods []string   // expected pod names in order
		expectedLens []int      // expected match lengths
	}{
		{
			name:      "Empty cache returns no matches",
			maxHashes: 100,
			topK:      3,
			model:     "test-model",
			pods: []*datastore.PodInfo{
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}},
			},
			queryHashes:  []uint64{1, 2, 3},
			expectedPods: []string{},
			expectedLens: []int{},
		},
		{
			name:      "Single pod exact match",
			maxHashes: 100,
			topK:      3,
			model:     "test-model",
			pods: []*datastore.PodInfo{
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}},
			},
			addHashes:    [][]uint64{{1, 2, 3}},
			queryHashes:  []uint64{1, 2, 3},
			expectedPods: []string{"ns1/pod1"},
			expectedLens: []int{3},
		},
		{
			name:      "Multiple pods with different match lengths",
			maxHashes: 100,
			topK:      3,
			model:     "test-model",
			pods: []*datastore.PodInfo{
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}},
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"}}},
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Namespace: "ns1"}}},
			},
			addHashes: [][]uint64{
				{1, 2, 3}, // pod1: full match
				{1, 2},    // pod2: partial match
				{1, 4, 5}, // pod3: single match
			},
			queryHashes:  []uint64{1, 2, 3},
			expectedPods: []string{"ns1/pod1", "ns1/pod2", "ns1/pod3"},
			expectedLens: []int{3, 2, 1},
		},
		{
			name:      "LRU eviction",
			maxHashes: 2, // Only allow 2 hashes
			topK:      3,
			model:     "test-model",
			pods: []*datastore.PodInfo{
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}},
			},
			addHashes:    [][]uint64{{1, 2, 3}}, // Add 3 hashes, one should be evicted
			queryHashes:  []uint64{1, 2, 3},
			expectedPods: []string{"ns1/pod1"},
			expectedLens: []int{2},
		},
		{
			name:      "LRU eviction with hashModelKey works correctly",
			maxHashes: 3, // Small capacity to force eviction
			topK:      5,
			model:     "eviction-model",
			pods: []*datastore.PodInfo{
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "eviction-pod", Namespace: "test"}}},
			},
			addHashes:    [][]uint64{{1, 2, 3, 4, 5}}, // Add more hashes than capacity
			queryHashes:  []uint64{1, 2, 3, 4, 5},
			expectedPods: []string{"test/eviction-pod"},
			expectedLens: []int{3}, // Only 3 hashes should remain due to LRU eviction
		},
		{
			name:      "Model isolation with same hash values",
			maxHashes: 10,
			topK:      5,
			model:     "isolated-model-1",
			pods: []*datastore.PodInfo{
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}},
				{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"}}},
			},
			addHashes:    [][]uint64{{999, 888, 777}}, // Only add to first pod
			queryHashes:  []uint64{999, 888, 777},
			expectedPods: []string{"ns1/pod1"},
			expectedLens: []int{3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockStore := datastore.New()
			store := NewModelPrefixStore(mockStore, tt.maxHashes, tt.topK)

			// Add pods to cache
			for i, pod := range tt.pods {
				if i < len(tt.addHashes) {
					store.Add(tt.model, tt.addHashes[i], pod)
				}
			}

			time.Sleep(50 * time.Millisecond) // Ensure cache evict cb has been run
			// Query matches
			matches := store.FindTopMatches(tt.model, tt.queryHashes, tt.pods)

			// Verify results
			if len(matches) != len(tt.expectedPods) {
				t.Errorf("got %d matches, want %d", len(matches), len(tt.expectedPods))
			}

			for i, expectedName := range tt.expectedPods {
				parts := strings.SplitN(expectedName, "/", 2)
				nsName := types.NamespacedName{Namespace: parts[0], Name: parts[1]}
				matchLen, ok := matches[nsName]
				if !ok {
					t.Errorf("expected pod %s not found in matches", expectedName)
					continue
				}
				if matchLen != tt.expectedLens[i] {
					t.Errorf("pod %s: got matchLen %d, want %d", expectedName, matchLen, tt.expectedLens[i])
				}
			}
		})
	}

	// Additional test cases that require special handling
	t.Run("Model isolation verification", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 10, 5)

		pod1 := &datastore.PodInfo{
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"},
			},
		}
		pod2 := &datastore.PodInfo{
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"},
			},
		}

		// Add same hash for different models
		store.Add("model-a", []uint64{100, 200}, pod1)
		store.Add("model-b", []uint64{100, 200}, pod2)

		// Query model-a should only return pod1
		matches1 := store.FindTopMatches("model-a", []uint64{100, 200}, []*datastore.PodInfo{pod1, pod2})
		assert.Equal(t, 1, len(matches1))
		assert.Equal(t, 2, matches1[types.NamespacedName{Namespace: "ns1", Name: "pod1"}])
		_, hasPod2 := matches1[types.NamespacedName{Namespace: "ns1", Name: "pod2"}]
		assert.False(t, hasPod2, "model-a should not match pod2")

		// Query model-b should only return pod2
		matches2 := store.FindTopMatches("model-b", []uint64{100, 200}, []*datastore.PodInfo{pod1, pod2})
		assert.Equal(t, 1, len(matches2))
		assert.Equal(t, 2, matches2[types.NamespacedName{Namespace: "ns1", Name: "pod2"}])
		_, hasPod1 := matches2[types.NamespacedName{Namespace: "ns1", Name: "pod1"}]
		assert.False(t, hasPod1, "model-b should not match pod1")

		// Non-existent model should return no matches
		matches3 := store.FindTopMatches("non-existent-model", []uint64{100, 200}, []*datastore.PodInfo{pod1, pod2})
		assert.Equal(t, 0, len(matches3))
	})

	t.Run("Eviction callback uses correct hashModelKey", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 3, 5) // Capacity of 3

		pod := &datastore.PodInfo{
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "callback-pod", Namespace: "test"},
			},
		}

		// Add 2 hashes for model-1 (LRU: [m1:1, m1:2])
		store.Add("callback-model-1", []uint64{1, 2}, pod)

		// Add 1 hash for model-2 (LRU: [m1:1, m1:2, m2:3])
		store.Add("callback-model-2", []uint64{3}, pod)

		// Add 2 more hashes for model-1, should trigger eviction (LRU: [m2:3, m1:5, m1:6])
		store.Add("callback-model-1", []uint64{5, 6}, pod)

		// Wait for eviction callbacks
		time.Sleep(100 * time.Millisecond)

		// Verify eviction worked correctly
		matches1Old := store.FindTopMatches("callback-model-1", []uint64{1, 2}, []*datastore.PodInfo{pod})
		matches1New := store.FindTopMatches("callback-model-1", []uint64{5, 6}, []*datastore.PodInfo{pod})
		matches2 := store.FindTopMatches("callback-model-2", []uint64{3}, []*datastore.PodInfo{pod})

		// Old hashes for model-1 should be evicted
		assert.Equal(t, 0, len(matches1Old), "Old model-1 hashes should be evicted")
		// New hashes for model-1 should remain
		assert.Equal(t, 1, len(matches1New), "New model-1 hashes should remain")
		podNsName := types.NamespacedName{Namespace: "test", Name: "callback-pod"}
		assert.Equal(t, 2, matches1New[podNsName], "Should match both new hashes")
		// Model-2 hash should remain (it wasn't evicted)
		assert.Equal(t, 1, len(matches2), "Model-2 hash should remain")
	})

	t.Run("Pod deletion cleans up all model entries", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 10, 5)

		podName := types.NamespacedName{Name: "deletion-pod", Namespace: "test"}
		pod := &datastore.PodInfo{
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName.Name, Namespace: podName.Namespace},
			},
		}

		// Add hashes for multiple models
		store.Add("deletion-model-1", []uint64{100, 200}, pod)
		store.Add("deletion-model-2", []uint64{300, 400}, pod)

		// Verify data exists
		matches1Before := store.FindTopMatches("deletion-model-1", []uint64{100, 200}, []*datastore.PodInfo{pod})
		matches2Before := store.FindTopMatches("deletion-model-2", []uint64{300, 400}, []*datastore.PodInfo{pod})
		assert.Equal(t, 1, len(matches1Before))
		assert.Equal(t, 1, len(matches2Before))

		// Simulate pod deletion
		store.onPodDeleted(datastore.EventData{
			EventType: datastore.EventDelete,
			Pod:       podName,
		})

		// Wait for cleanup
		time.Sleep(100 * time.Millisecond)

		// Verify all data for this pod is cleaned up from both models
		matches1After := store.FindTopMatches("deletion-model-1", []uint64{100, 200}, []*datastore.PodInfo{pod})
		matches2After := store.FindTopMatches("deletion-model-2", []uint64{300, 400}, []*datastore.PodInfo{pod})
		assert.Equal(t, 0, len(matches1After))
		assert.Equal(t, 0, len(matches2After))
		assert.Equal(t, 0, len(store.entries))
	})

	t.Run("Candidate pod filtering: non-candidate cached pods are excluded", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 10, 5)

		pod1 := &datastore.PodInfo{
			Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}},
		}
		pod2 := &datastore.PodInfo{
			Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"}},
		}

		// Both pods are added to the cache with the same hashes.
		store.Add("filter-model", []uint64{1, 2, 3}, pod1)
		store.Add("filter-model", []uint64{1, 2, 3}, pod2)

		// Query with only pod2 as the candidate – pod1 must NOT appear in results.
		matches := store.FindTopMatches("filter-model", []uint64{1, 2, 3}, []*datastore.PodInfo{pod2})
		assert.Equal(t, 1, len(matches), "only the candidate pod should be returned")
		pod2Key := types.NamespacedName{Namespace: "ns1", Name: "pod2"}
		assert.Equal(t, 3, matches[pod2Key])
		_, hasPod1 := matches[types.NamespacedName{Namespace: "ns1", Name: "pod1"}]
		assert.False(t, hasPod1)

		// Query with only pod1 as the candidate – pod2 must NOT appear in results.
		matches = store.FindTopMatches("filter-model", []uint64{1, 2, 3}, []*datastore.PodInfo{pod1})
		assert.Equal(t, 1, len(matches), "only the candidate pod should be returned")
		pod1Key := types.NamespacedName{Namespace: "ns1", Name: "pod1"}
		assert.Equal(t, 3, matches[pod1Key])
		_, hasPod2 := matches[types.NamespacedName{Namespace: "ns1", Name: "pod2"}]
		assert.False(t, hasPod2)
	})

	t.Run("Candidate pod filtering: empty candidates list yields no results", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 10, 5)

		pod := &datastore.PodInfo{
			Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}},
		}
		store.Add("filter-model", []uint64{1, 2, 3}, pod)

		// Even though pod is cached, an empty candidates list must return nothing.
		matches := store.FindTopMatches("filter-model", []uint64{1, 2, 3}, []*datastore.PodInfo{})
		assert.Equal(t, 0, len(matches), "empty candidate list must yield no results")
	})
}

func TestModelPrefixStoreConcurrency(t *testing.T) {
	t.Run("Concurrent Add operations", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 100, 10)

		const numGoroutines = 50
		const numHashesPerPod = 10

		// Create pods
		pods := make([]*datastore.PodInfo, numGoroutines)
		for i := range numGoroutines {
			pods[i] = &datastore.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod%d", i),
						Namespace: "test",
					},
				},
			}
		}

		// Concurrently add hashes
		wg := sync.WaitGroup{}
		wg.Add(numGoroutines)
		for i := range numGoroutines {
			go func(podIndex int) {
				defer wg.Done()

				// Generate hashes for this pod
				hashes := make([]uint64, numHashesPerPod)
				for j := range numHashesPerPod {
					hashes[j] = uint64(podIndex*numHashesPerPod + j + 1)
				}

				store.Add("test-model", hashes, pods[podIndex])
			}(i)
		}

		// Wait for all goroutines to complete
		wg.Wait()

		// Verify data integrity - query for some hashes
		queryHashes := []uint64{1, 2, 3, 4, 5}
		matches := store.FindTopMatches("test-model", queryHashes, pods)
		assert.Equal(t, 1, len(matches))
	})

	t.Run("Concurrent Add and FindTopMatches", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 100, 5)

		const numWriters = 20
		const numReaders = 30

		// Create pods
		pods := make([]*datastore.PodInfo, numWriters)
		for i := range numWriters {
			pods[i] = &datastore.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("writer-pod%d", i),
						Namespace: "test",
					},
				},
			}
		}

		var wg sync.WaitGroup
		wg.Add(numWriters + numReaders)
		// Start writers
		for i := range numWriters {
			go func(podIndex int) {
				defer wg.Done()
				hashes := []uint64{
					uint64(podIndex + 1),
					uint64(podIndex + 2),
					uint64(podIndex + 3),
				}
				store.Add("concurrent-model", hashes, pods[podIndex])
			}(i)
		}

		// Start readers
		for range numReaders {
			go func() {
				defer wg.Done()

				queryHashes := []uint64{1, 2, 3, 4, 5}
				matches := store.FindTopMatches("concurrent-model", queryHashes, pods)
				_ = matches // Just consume the result
			}()
		}

		// Let them run for a while
		wg.Wait()

		// Final verification
		queryHashes := []uint64{1, 2, 3}
		matches := store.FindTopMatches("concurrent-model", queryHashes, pods)
		// Should not panic and should return valid results
		for _, matchLen := range matches {
			if matchLen <= 0 {
				t.Errorf("Invalid match length: %d", matchLen)
			}
		}
	})

	t.Run("Concurrent Add and Pod Deletion", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 50, 10)

		const numPods = 20
		const numOperations = 50

		// Create pods
		pods := make([]*datastore.PodInfo, numPods)
		for i := range numPods {
			pods[i] = &datastore.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("deletion-pod%d", i),
						Namespace: "test",
					},
				},
			}
		}

		var wg sync.WaitGroup

		// Add operations
		for i := range numOperations {
			wg.Add(1)
			go func(opIndex int) {
				defer wg.Done()
				podIndex := opIndex % numPods
				hashes := []uint64{
					uint64(opIndex + 1),
					uint64(opIndex + 2),
					uint64(opIndex + 3),
				}
				store.Add("deletion-model", hashes, pods[podIndex])
			}(i)
		}

		time.Sleep(10 * time.Millisecond) // Let some adds happen first

		// Pod deletion operations
		for i := range numPods / 2 {
			wg.Add(1)
			go func(podIndex int) {
				defer wg.Done()

				nsName := types.NamespacedName{
					Namespace: pods[podIndex].Pod.Namespace,
					Name:      pods[podIndex].Pod.Name,
				}

				// Simulate pod deletion event
				store.onPodDeleted(datastore.EventData{
					EventType: datastore.EventDelete,
					Pod:       nsName,
				})
			}(i)
		}

		wg.Wait()
		time.Sleep(10 * time.Millisecond)

		queryHashes := []uint64{1, 2, 3}
		matches := store.FindTopMatches("deletion-model", queryHashes, pods)
		assert.Equal(t, 0, len(matches))
	})

	t.Run("LRU Eviction Under Concurrency", func(t *testing.T) {
		mockStore := datastore.New()
		// Small capacity to force frequent evictions
		store := NewModelPrefixStore(mockStore, 5, 10)

		const numPods = 10
		const numOperations = 100

		// Create pods
		pods := make([]*datastore.PodInfo, numPods)
		for i := range numPods {
			pods[i] = &datastore.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("eviction-pod%d", i),
						Namespace: "test",
					},
				},
			}
		}

		var wg sync.WaitGroup

		// Concurrent operations that will trigger evictions
		for i := range numOperations {
			wg.Add(1)
			go func(opIndex int) {
				defer wg.Done()
				podIndex := opIndex % numPods

				// Generate many hashes to trigger evictions
				hashes := make([]uint64, 10)
				for j := range 10 {
					hashes[j] = uint64(opIndex*10 + j + 1)
				}

				store.Add("eviction-model", hashes, pods[podIndex])

				// Also do some queries
				if opIndex%3 == 0 {
					queryHashes := []uint64{
						uint64(opIndex + 1),
						uint64(opIndex + 2),
					}
					matches := store.FindTopMatches("eviction-model", queryHashes, pods)
					_ = matches
				}
			}(i)
		}

		wg.Wait()

		// Wait for all eviction callbacks to complete
		time.Sleep(100 * time.Millisecond)

		// Verify cache is still functional
		for i := range numPods {
			store.podHashesMu.RLock()
			cacheLen := store.podHashes[types.NamespacedName{Namespace: pods[i].Pod.Namespace, Name: pods[i].Pod.Name}].Len()
			assert.Equal(t, 5, cacheLen)
			store.podHashesMu.RUnlock()
		}
	})

	t.Run("High Load Stress Test", func(t *testing.T) {
		mockStore := datastore.New()
		store := NewModelPrefixStore(mockStore, 100, 20)

		const numModels = 5
		const numPodsPerModel = 10
		const numGoroutines = 100
		const duration = 200 * time.Millisecond

		// Create pods for each model
		allPods := make([][]*datastore.PodInfo, numModels)
		for m := range numModels {
			allPods[m] = make([]*datastore.PodInfo, numPodsPerModel)
			for p := range numPodsPerModel {
				allPods[m][p] = &datastore.PodInfo{
					Pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("stress-model%d-pod%d", m, p),
							Namespace: "test",
						},
					},
				}
			}
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup

		// Mixed workload: adds, queries, and deletions
		for i := range numGoroutines {
			wg.Add(1)
			go func(goroutineID int) {
				defer wg.Done()

				for {
					select {
					case <-stop:
						return
					default:
						modelIndex := goroutineID % numModels
						podIndex := goroutineID % numPodsPerModel
						modelName := fmt.Sprintf("stress-model-%d", modelIndex)

						operation := goroutineID % 4
						switch operation {
						case 0, 1: // Add operations (50% of the time)
							hashes := []uint64{
								uint64(goroutineID + 1),
								uint64(goroutineID + 2),
								uint64(goroutineID + 3),
							}
							store.Add(modelName, hashes, allPods[modelIndex][podIndex])

						case 2: // Query operations (25% of the time)
							queryHashes := []uint64{
								uint64((goroutineID % 50) + 1),
								uint64((goroutineID % 50) + 2),
							}
							matches := store.FindTopMatches(modelName, queryHashes, allPods[modelIndex])
							_ = matches

						case 3: // Pod deletion (25% of the time)
							if goroutineID%10 == 0 { // Only some goroutines do deletions
								nsName := types.NamespacedName{
									Namespace: allPods[modelIndex][podIndex].Pod.Namespace,
									Name:      allPods[modelIndex][podIndex].Pod.Name,
								}
								store.onPodDeleted(datastore.EventData{
									EventType: datastore.EventDelete,
									Pod:       nsName,
								})
							}
						}

						// Small delay to avoid overwhelming
						if goroutineID%10 == 0 {
							time.Sleep(time.Microsecond * 100)
						}
					}
				}
			}(i)
		}

		// Let the stress test run
		time.Sleep(duration)
		close(stop)
		wg.Wait()

		// Wait for cleanup operations
		time.Sleep(50 * time.Millisecond)

		// Final verification - cache should still be functional
		for m := range numModels {
			modelName := fmt.Sprintf("stress-model-%d", m)
			queryHashes := []uint64{4, 5, 6}
			matches := store.FindTopMatches(modelName, queryHashes, allPods[m])
			t.Logf("Model %s: found %d matches after stress test", modelName, len(matches))
		}
	})
}

func BenchmarkModelPrefixStore_FindAndAdd(b *testing.B) {
	mockStore := datastore.New()
	store := NewModelPrefixStore(mockStore, 500, 15)

	const numPods = 100
	const numModels = 3

	// Pre-generate test data
	pods := make([]*datastore.PodInfo, numPods)
	hashSets := make([][][]uint64, numModels)
	queryPatterns := make([][]uint64, numModels)

	for i := 0; i < numPods; i++ {
		pods[i] = &datastore.PodInfo{
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("mixed-pod-%d", i),
					Namespace: fmt.Sprintf("mixed-ns-%d", i%5),
				},
			},
		}
	}

	for m := 0; m < numModels; m++ {
		hashSets[m] = make([][]uint64, numPods)
		for i := 0; i < numPods; i++ {
			hashSets[m][i] = make([]uint64, 40)
			for j := 0; j < 40; j++ {
				if j < 8 {
					hashSets[m][i][j] = uint64(m*20 + j + 1)
				} else {
					hashSets[m][i][j] = uint64(i*1000 + j + m*100)
				}
			}
		}

		queryPatterns[m] = make([]uint64, 25)
		for j := 0; j < 25; j++ {
			if j < 8 {
				queryPatterns[m][j] = uint64(m*20 + j + 1)
			} else {
				queryPatterns[m][j] = uint64(m*200 + j)
			}
		}
	}

	concurrency := 50

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		wg := sync.WaitGroup{}
		wg.Add(concurrency)
		for j := 0; j < concurrency; j++ {
			go func(i int) {
				defer wg.Done()
				podIdx := i % numPods
				modelIdx := i % numModels
				modelName := fmt.Sprintf("mixed-model-%d", modelIdx)
				// FindTopMatches operation
				_ = store.FindTopMatches(modelName, queryPatterns[modelIdx], pods)
				store.Add(modelName, hashSets[modelIdx][podIdx], pods[podIdx])
			}(i + j)
		}
		wg.Wait()
	}
}
