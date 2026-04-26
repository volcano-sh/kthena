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

package autoscaler

import (
	"testing"

	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
)

func TestSplitPDReplicas_WithRatioAndBounds(t *testing.T) {
	tests := []struct {
		name          string
		total         int32
		ratio         string
		prefillMin    int32
		prefillMax    int32
		decodeMin     int32
		decodeMax     int32
		currentPrefill int32
		currentDecode int32
		wantPrefill   int32
		wantDecode    int32
	}{
		{
			name:          "ratio 1:2 normal split",
			total:         9,
			ratio:         "1:2",
			prefillMin:    1,
			prefillMax:    8,
			decodeMin:     2,
			decodeMax:     16,
			currentPrefill: 3,
			currentDecode: 6,
			wantPrefill:   3,
			wantDecode:    6,
		},
		{
			name:          "prefill max bound hit then overflow goes to decode",
			total:         10,
			ratio:         "3:1",
			prefillMin:    1,
			prefillMax:    5,
			decodeMin:     1,
			decodeMax:     10,
			currentPrefill: 2,
			currentDecode: 2,
			wantPrefill:   5,
			wantDecode:    5,
		},
		{
			name:          "decode min bound hit",
			total:         5,
			ratio:         "4:1",
			prefillMin:    1,
			prefillMax:    10,
			decodeMin:     2,
			decodeMax:     10,
			currentPrefill: 3,
			currentDecode: 2,
			wantPrefill:   3,
			wantDecode:    2,
		},
		{
			name:          "empty ratio falls back to current ratio",
			total:         6,
			ratio:         "",
			prefillMin:    1,
			prefillMax:    10,
			decodeMin:     1,
			decodeMax:     10,
			currentPrefill: 2,
			currentDecode: 4,
			wantPrefill:   2,
			wantDecode:    4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPrefill, gotDecode, err := SplitPDReplicas(
				tt.total,
				tt.ratio,
				tt.prefillMin,
				tt.prefillMax,
				tt.decodeMin,
				tt.decodeMax,
				tt.currentPrefill,
				tt.currentDecode,
			)
			if err != nil {
				t.Fatalf("SplitPDReplicas returned error: %v", err)
			}
			if gotPrefill != tt.wantPrefill || gotDecode != tt.wantDecode {
				t.Fatalf("expected split %d:%d, got %d:%d", tt.wantPrefill, tt.wantDecode, gotPrefill, gotDecode)
			}
			if gotPrefill+gotDecode != tt.total {
				t.Fatalf("split should preserve total=%d, got %d", tt.total, gotPrefill+gotDecode)
			}
		})
	}
}

func TestSplitPDReplicas_InvalidRatio(t *testing.T) {
	_, _, err := SplitPDReplicas(3, "1:x", 1, 10, 1, 10, 1, 2)
	if err == nil {
		t.Fatal("expected ratio parse error, got nil")
	}
}

func TestMergeMetrics(t *testing.T) {
	merged := mergeMetrics(
		algorithm.Metrics{"load": 2, "queue": 1},
		algorithm.Metrics{"load": 3, "queue": 4},
	)
	if merged["load"] != 5 {
		t.Fatalf("expected load=5, got %v", merged["load"])
	}
	if merged["queue"] != 5 {
		t.Fatalf("expected queue=5, got %v", merged["queue"])
	}
}
