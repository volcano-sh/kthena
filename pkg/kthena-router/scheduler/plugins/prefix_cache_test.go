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
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cespare/xxhash"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
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
			Prompt: &common.ChatMessage{Text: prompt},
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
			Prompt: &common.ChatMessage{}, // empty – no Text, no Messages
		}
		scores := plugin.Score(ctx, []*datastore.PodInfo{pod})
		if scores != nil {
			t.Errorf("expected nil for empty prompt, got %v", scores)
		}
	})
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func histSampleSum(t *testing.T, vec *prometheus.HistogramVec, lvs ...string) float64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

func histSampleCount(t *testing.T, vec *prometheus.HistogramVec, lvs ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// histBucketCount returns the cumulative count of the bucket whose upper bound equals le.
func histBucketCount(t *testing.T, vec *prometheus.HistogramVec, le float64, lvs ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, b := range m.GetHistogram().GetBucket() {
		if b.GetUpperBound() == le {
			return b.GetCumulativeCount()
		}
	}
	t.Fatalf("bucket le=%v not found", le)
	return 0
}

func TestMatchRatio(t *testing.T) {
	cases := []struct {
		matched, total int
		want           float64
	}{
		{7, 13, 7.0 / 13.0}, // partial — guards against a numerator/denominator swap
		{0, 13, 0},          // miss
		{13, 13, 1},         // full match
		{5, 0, 0},           // no blocks — must not divide by zero
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := matchRatio(c.matched, c.total); got != c.want {
			t.Errorf("matchRatio(%d, %d) = %v, want %v", c.matched, c.total, got, c.want)
		}
	}
}

func TestPrefixCacheScoreMetrics(t *testing.T) {
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

	const model = "prefixmetrics-model"
	prompt := "hello world from prefix cache metrics test"
	hashes := plugin.hashPrompt(model, prompt)
	prefixStore.Add(model, hashes, pod1)

	recorder := metrics.NewRequestMetricsRecorder(metrics.DefaultMetrics, model, "/v1/chat/completions")

	countBefore := histSampleCount(t, &metrics.DefaultMetrics.PrefixCacheMatchRatio, model)
	missBefore := histBucketCount(t, &metrics.DefaultMetrics.PrefixCacheMatchRatio, 0, model)
	sumBefore := histSampleSum(t, &metrics.DefaultMetrics.PrefixCacheMatchRatio, model)

	// Matching prompt -> full self-match, ratio == 1.0.
	hitCtx := &framework.Context{Model: model, Prompt: &common.ChatMessage{Text: prompt}, MetricsRecorder: recorder}
	plugin.Score(hitCtx, []*datastore.PodInfo{pod1, pod2})

	// Non-matching prompt -> ratio 0 (lands in the le=0 bucket).
	missCtx := &framework.Context{Model: model, Prompt: &common.ChatMessage{Text: "a completely different prompt"}, MetricsRecorder: recorder}
	plugin.Score(missCtx, []*datastore.PodInfo{pod1, pod2})

	// Recorded values: 1.0 (full match) + 0.0 (miss) = 1.0.
	if got := histSampleSum(t, &metrics.DefaultMetrics.PrefixCacheMatchRatio, model) - sumBefore; got != 1.0 {
		t.Errorf("prefix cache match_ratio sum delta = %v, want 1.0", got)
	}

	if got := histSampleCount(t, &metrics.DefaultMetrics.PrefixCacheMatchRatio, model) - countBefore; got != 2 {
		t.Errorf("prefix cache match_ratio sample count delta = %d, want 2", got)
	}
	if got := histBucketCount(t, &metrics.DefaultMetrics.PrefixCacheMatchRatio, 0, model) - missBefore; got != 1 {
		t.Errorf("prefix cache match_ratio le=0 (miss) delta = %d, want 1", got)
	}
}

func TestPrefixCacheEntriesProviderRegistered(t *testing.T) {
	mockDS := datastore.New()
	plugin := NewPrefixCache(mockDS, runtime.RawExtension{Raw: []byte{}})

	pod := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}}
	const model = "prefixentries-model"
	hashes := plugin.hashPrompt(model, "some prompt that yields a single block")
	plugin.store.Add(model, hashes, pod)

	if got := plugin.store.EntryCount(); got < 1 {
		t.Fatalf("EntryCount() = %v, want >= 1 after Add", got)
	}
}

func benchmarkPrefixScore(b *testing.B, withMetrics bool) {
	mockDS := datastore.New()
	prefixStore := cache.NewModelPrefixStore(mockDS, 100000, 5)
	plugin := &PrefixCache{
		name:             PrefixCachePluginName,
		blockSizeToHash:  64,
		maxBlocksToMatch: 128,
		store:            prefixStore,
	}
	pod1 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}}
	pod2 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"}}}
	pods := []*datastore.PodInfo{pod1, pod2}

	const model = "bench-prefix-model"
	prompt := strings.Repeat("the quick brown fox jumps over the lazy dog ", 40)
	hashes := plugin.hashPrompt(model, prompt)
	prefixStore.Add(model, hashes, pod1)

	ctx := &framework.Context{Model: model, Prompt: &common.ChatMessage{Text: prompt}}
	if withMetrics {
		ctx.MetricsRecorder = metrics.NewRequestMetricsRecorder(metrics.DefaultMetrics, model, "/v1/chat/completions")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plugin.Score(ctx, pods)
	}
}

func BenchmarkPrefixCacheScore_NoMetrics(b *testing.B)   { benchmarkPrefixScore(b, false) }
func BenchmarkPrefixCacheScore_WithMetrics(b *testing.B) { benchmarkPrefixScore(b, true) }

func TestNewPrefixCacheWithEmptyArgs(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()

	var logBuffer bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logBuffer)

	plugin := NewPrefixCache(datastore.New(), runtime.RawExtension{Raw: []byte{}})
	klog.Flush()

	if plugin.blockSizeToHash != 64 {
		t.Fatalf("unexpected default blockSizeToHash: got %d, want %d", plugin.blockSizeToHash, 64)
	}
	if plugin.maxBlocksToMatch != 128 {
		t.Fatalf("unexpected default maxBlocksToMatch: got %d, want %d", plugin.maxBlocksToMatch, 128)
	}
	if strings.Contains(logBuffer.String(), "Failed to unmarshal PrefixCacheArgs") {
		t.Fatalf("expected no unmarshal error log for empty args, got: %s", logBuffer.String())
	}
}

func TestNewPrefixCacheRespectsTopKMatches(t *testing.T) {
	plugin := NewPrefixCache(datastore.New(), runtime.RawExtension{
		Raw: []byte(`{"blockSizeToHash": 64, "maxBlocksToMatch": 128, "maxHashCacheSize": 50000, "topKMatches": 1}`),
	})

	pod1 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}}
	pod2 := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns1"}}}

	prompt := "same prompt for both pods"
	hashes := plugin.hashPrompt("test-model", prompt)
	plugin.store.Add("test-model", hashes, pod1)
	plugin.store.Add("test-model", hashes, pod2)

	ctx := &framework.Context{
		Model:  "test-model",
		Prompt: &common.ChatMessage{Text: prompt},
	}
	scores := plugin.Score(ctx, []*datastore.PodInfo{pod1, pod2})

	nonZero := 0
	for _, score := range scores {
		if score > 0 {
			nonZero++
		}
	}
	if nonZero != 1 {
		t.Fatalf("expected exactly 1 pod with non-zero score when topKMatches=1, got %d", nonZero)
	}
}

// TestHashPromptEquivalence freezes the hash chain for a full 128-block prompt
// (the production hot path). These constants are implementation-independent:
// they were produced by the original fmt.Sprintf-based hashing and must stay
// identical, since any drift breaks cross-replica prefix-cache hits.
func TestHashPromptEquivalence(t *testing.T) {
	// 8192 bytes = 128 blocks of 64, cycling a-z (deterministic across runs).
	var promptBuf strings.Builder
	promptBuf.Grow(8192)
	for i := 0; i < 8192; i++ {
		promptBuf.WriteByte(byte('a' + (i % 26)))
	}
	prompt := promptBuf.String()

	p := &PrefixCache{
		blockSizeToHash:  64,
		maxBlocksToMatch: 128,
	}
	got := p.hashPrompt("bench-model", prompt)

	golden := []uint64{2255003460060045391, 11149546857834002314, 17187590233846641107, 4034322412968604605, 15207223297201143248, 9824985680695740636, 494137533886476659, 12428692248081375853, 3281587933229426645, 11281488786797834782, 11430871844812671089, 15813208048949774556, 9363747689914547027, 11386586896606693646, 16254268837856693350, 11227496801155794112, 5552414806859681509, 4028469627393635033, 13204935343603999316, 9178657671534218611, 8315128942934556500, 729494013592457529, 6174936825905842805, 895484040459390085, 5570976399983798947, 10864533163928278681, 7760174028370662776, 7008975524827789467, 2785119036091335614, 6439212343810728060, 13393469611375930812, 13793228971756517991, 11175323166057494155, 14010041459816744473, 7203502563662825803, 10283166783258117551, 1954552144515689675, 13246579544777422362, 13202498421803114079, 11876666485791327302, 11661352576924688786, 16140613343339141160, 5889389170009119976, 10701204005199758260, 1280293580992457147, 4820153341329627974, 5911718727292447357, 11228888571946781470, 10583022672352952901, 16121550968138304185, 17245035097495660264, 16221945840124327571, 10880571561823988553, 8162986918409139638, 11358347241429614479, 11524963193749263358, 8049849156529785307, 5107960806196807254, 14168434245139972826, 14853881945155960584, 5321100836073231020, 1944816818447785456, 2350124915713761971, 9474821423694415191, 8058853764834082289, 12420817111793910912, 2418051801207693879, 12657679937803826462, 17041349994164055071, 3686477523709277737, 15393923414729048344, 3790424095685402591, 6923990074150558368, 11694554354790678266, 3968519603595637314, 17502932821980345766, 7378712720441818257, 168382028294090910, 15273426704999976206, 17446103999652081165, 14359780171923841468, 7523250232915281961, 1920120734516321188, 341678210645398961, 15874329493408057713, 1130855822629327650, 15640208548837393180, 2265430572041035215, 11823681631912316344, 215526461674733228, 16701895397045398205, 10917398329548690148, 17889007605708178196, 17100116311457151354, 7672738113594452170, 15147150116550744013, 12417697592182920225, 5476664805198487136, 13147904999979788806, 5965418003257176534, 6284956858947182112, 18341029568316616176, 12109952977081839263, 9130609089793203462, 16420324086051396503, 5569412897995944916, 1537929088821991852, 11984408485833614553, 14650686116475342169, 15145595127895359981, 12461251480288579572, 96299960962600125, 15208096417347002753, 5262276131021051437, 5929265978246150264, 13679110378923766846, 4901795099007486237, 5930661544815874025, 2205972921645543346, 17003871549387556553, 16050761663753432336, 5238550911068359565, 14367258910746172805, 1810149288675663232, 871172192369462803, 13880238502922240587, 6735955785914442710, 3713727362220958295}

	if len(got) != len(golden) {
		t.Fatalf("hash count = %d, want %d (chain shape changed)", len(got), len(golden))
	}
	for i := range golden {
		if got[i] != golden[i] {
			t.Fatalf("hash[%d] = %d, want %d (hash chain diverged from frozen values)", i, got[i], golden[i])
		}
	}
}

// TestHashPromptResultCapacity pins the result slice to the blocks the prompt
// produces. Sizing it at maxBlocksToMatch instead gives every prompt the full
// backing array, which costs short prompts far more than the growth it avoids.
func TestHashPromptResultCapacity(t *testing.T) {
	p := &PrefixCache{
		blockSizeToHash:  64,
		maxBlocksToMatch: 128,
	}

	// Block counts are deliberately not powers of two. Growing the slice by append
	// lands on a power-of-two capacity, so these are the lengths where a grown
	// result and a sized one differ.
	cases := []struct {
		promptLen  int
		wantHashes int
	}{
		{192, 3},
		{320, 5},
		{1600, 25},
		{32768, 128}, // capped at maxBlocksToMatch
	}
	for _, tc := range cases {
		got := p.hashPrompt("test-model", strings.Repeat("a", tc.promptLen))
		if len(got) != tc.wantHashes {
			t.Fatalf("prompt %dB: got %d hashes, want %d", tc.promptLen, len(got), tc.wantHashes)
		}
		if cap(got) != tc.wantHashes {
			t.Fatalf("prompt %dB: result cap %d, want %d", tc.promptLen, cap(got), tc.wantHashes)
		}
	}
}

// BenchmarkHashPrompt isolates the per-request hashing allocation, the data-plane
// hot path. b.ReportAllocs surfaces the per-op allocations eliminated by the
// strconv.AppendUint buffer reuse and by sizing the result slice up front. Sizes
// span a single block to beyond maxBlocksToMatch.
func BenchmarkHashPrompt(b *testing.B) {
	p := &PrefixCache{
		blockSizeToHash:  64,
		maxBlocksToMatch: 128,
	}

	for _, promptLen := range []int{64, 512, 2048, 8192, 32768} {
		var promptBuf strings.Builder
		promptBuf.Grow(promptLen)
		for i := 0; i < promptLen; i++ {
			promptBuf.WriteByte(byte('a' + (i % 26)))
		}
		prompt := promptBuf.String()

		b.Run(fmt.Sprintf("prompt=%dB", promptLen), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.hashPrompt("bench-model", prompt)
			}
		})
	}
}
