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

package algorithm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/datastructure"
)

// neverExpireMs is a TTL large enough that no window entry expires during tests.
// Must stay well below MaxInt64/2 because LineChartSlidingWindow internally
// computes maxDriftingMilliseconds = 2 * freshMilliseconds; overflow would
// cause drifting values to expire immediately.
const neverExpireMs = int64(1e15) // ~31 years in milliseconds

func int32Ptr(v int32) *int32 { return &v }

func emptyHistory() *History {
	return &History{
		MaxRecommendation:     datastructure.NewMaximumRecordSlidingWindow[int32](neverExpireMs),
		MinRecommendation:     datastructure.NewMinimumRecordSlidingWindow[int32](neverExpireMs),
		MaxCorrected:          datastructure.NewMaximumLineChartSlidingWindow[int32](neverExpireMs),
		MinCorrectedForStable: datastructure.NewMinimumLineChartSlidingWindow[int32](neverExpireMs),
		MinCorrectedForPanic:  datastructure.NewMinimumLineChartSlidingWindow[int32](neverExpireMs),
	}
}

// makeBehavior constructs an AutoscalingPolicyBehavior from the most commonly
// varied fields, keeping test cases concise.
func makeBehavior(
	scaleDownInstances, scaleDownPercent int32, scaleDownSelect v1alpha1.SelectPolicyType,
	scaleUpInstances, scaleUpPercent int32, scaleUpSelect v1alpha1.SelectPolicyType,
	panicPercent int32,
) *v1alpha1.AutoscalingPolicyBehavior {
	return &v1alpha1.AutoscalingPolicyBehavior{
		ScaleDown: v1alpha1.AutoscalingPolicyStablePolicy{
			Instances:    int32Ptr(scaleDownInstances),
			Percent:      int32Ptr(scaleDownPercent),
			SelectPolicy: scaleDownSelect,
		},
		ScaleUp: v1alpha1.AutoscalingPolicyScaleUpPolicy{
			StablePolicy: v1alpha1.AutoscalingPolicyStablePolicy{
				Instances:    int32Ptr(scaleUpInstances),
				Percent:      int32Ptr(scaleUpPercent),
				SelectPolicy: scaleUpSelect,
			},
			PanicPolicy: v1alpha1.AutoscalingPolicyPanicPolicy{
				Percent: int32Ptr(panicPercent),
			},
		},
	}
}

func TestGetCorrectedInstances(t *testing.T) {
	type TestCase struct {
		name              string
		args              CorrectedInstancesAlgorithm
		expectedCorrected int32
	}

	testcases := []TestCase{
		// ── stable mode – no scaling needed ──────────────────────────────────
		{
			name: "when stable and recommended equals current instances then return current",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         20,
				CurrentInstances:     7,
				RecommendedInstances: 7,
			},
			expectedCorrected: 7,
		},
		// ── stable mode – scale down ──────────────────────────────────────────
		{
			// SelectPolicyOr picks min(abs, rel) → less-restrictive of the two
			// constraints; with no history pastSample=current=10, abs=9, rel=6,
			// constraint=6; corrected=max(5,6)=6→min(6,10)=6.
			name: "when stable scale down and select policy Or then less restrictive constraint applied",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(1, 40, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         20,
				CurrentInstances:     10,
				RecommendedInstances: 5,
			},
			expectedCorrected: 6,
		},
		{
			// SelectPolicyAnd picks max(abs, rel) → more-restrictive of the two
			// constraints; same inputs: abs=9, rel=6, constraint=9; corrected=9.
			name: "when stable scale down and select policy And then more restrictive constraint applied",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(1, 40, v1alpha1.SelectPolicyAnd, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         20,
				CurrentInstances:     10,
				RecommendedInstances: 5,
			},
			expectedCorrected: 9,
		},
		{
			// Unknown SelectPolicy falls through to constraint=math.MinInt32, so
			// no lower bound is enforced and the raw recommendation is used.
			name: "when stable scale down and unknown select policy then no constraint applied",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(1, 40, "Unknown", 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         20,
				CurrentInstances:     10,
				RecommendedInstances: 5,
			},
			expectedCorrected: 5,
		},
		{
			// A non-empty MaxRecommendation window raises corrected above the raw
			// recommendation before other constraints are applied.
			// MaxRec=9 → corrected=max(5,9)=9; pastSample=10,abs=8,rel=8,Or→8;
			// corrected=max(9,8)=9→min(9,10)=9.
			name: "when stable scale down and max recommendation history is higher then better recommendation preferred",
			args: CorrectedInstancesAlgorithm{
				IsPanic: false,
				History: func() *History {
					h := emptyHistory()
					h.MaxRecommendation.Append(9)
					return h
				}(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         20,
				CurrentInstances:     10,
				RecommendedInstances: 5,
			},
			expectedCorrected: 9,
		},
		{
			// A MaxCorrected history value larger than current raises pastSample,
			// which tightens the scale-down constraint.
			// MaxCorrected=12 → pastSample=12, abs=9, rel=9, Or→9;
			// corrected=max(3,9)=9→min(9,10)=9.
			name: "when stable scale down and max corrected history is larger then constraint tightens",
			args: CorrectedInstancesAlgorithm{
				IsPanic: false,
				History: func() *History {
					h := emptyHistory()
					h.MaxCorrected.Append(12)
					return h
				}(),
				Behavior:             makeBehavior(3, 25, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         20,
				CurrentInstances:     10,
				RecommendedInstances: 3,
			},
			expectedCorrected: 9,
		},
		// ── stable mode – scale up ────────────────────────────────────────────
		{
			// SelectPolicyOr picks max(abs, rel); with pastSample=current=5,
			// abs=8, rel=10, constraint=10; corrected=min(20,10)=10→max(10,5)=10.
			name: "when stable scale up and select policy Or then less restrictive constraint applied",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 3, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     5,
				RecommendedInstances: 20,
			},
			expectedCorrected: 10,
		},
		{
			// SelectPolicyAnd picks min(abs, rel); abs=8, rel=10, constraint=8;
			// corrected=min(20,8)=8→max(8,5)=8.
			name: "when stable scale up and select policy And then more restrictive constraint applied",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 3, 100, v1alpha1.SelectPolicyAnd, 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     5,
				RecommendedInstances: 20,
			},
			expectedCorrected: 8,
		},
		{
			// Unknown SelectPolicy falls through to constraint=math.MaxInt32, so
			// the full recommendation is returned unconstrained.
			name: "when stable scale up and unknown select policy then no constraint applied",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 3, 100, "Unknown", 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     5,
				RecommendedInstances: 20,
			},
			expectedCorrected: 20,
		},
		{
			// MinRecommendation=12 pulls corrected down before the rate constraint:
			// corrected=min(20,12)=12; then rate limit with pastSample=5,Or→10;
			// corrected=min(12,10)=10→max(10,5)=10.
			name: "when stable scale up and min recommendation history is lower then better recommendation preferred",
			args: CorrectedInstancesAlgorithm{
				IsPanic: false,
				History: func() *History {
					h := emptyHistory()
					h.MinRecommendation.Append(12)
					return h
				}(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 3, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     5,
				RecommendedInstances: 20,
			},
			expectedCorrected: 10,
		},
		{
			// A MinCorrectedForStable history smaller than current lowers pastSample,
			// which tightens the scale-up rate limit.
			// MinCorrected=6 → pastSample=min(6,10)=6; abs=8, rel=9, Or→9;
			// corrected=min(50,9)=9→max(9,10)=10.
			name: "when stable scale up and min corrected history is smaller then constraint tightens",
			args: CorrectedInstancesAlgorithm{
				IsPanic: false,
				History: func() *History {
					h := emptyHistory()
					h.MinCorrectedForStable.Append(6)
					return h
				}(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 50, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     10,
				RecommendedInstances: 50,
			},
			expectedCorrected: 10,
		},
		// ── panic mode ────────────────────────────────────────────────────────
		{
			// At 1000% the relative constraint is far above recommended, so the
			// full recommendation is returned.
			// pastSample=5, relConst=5+50=55; corrected=min(15,55)=15→max(15,5)=15.
			name: "when panic and panic percent is 1000 then constraint does not limit growth",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              true,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     5,
				RecommendedInstances: 15,
			},
			expectedCorrected: 15,
		},
		{
			// At 100% the relative constraint equals current*2, limiting growth.
			// pastSample=5, relConst=5+5=10; corrected=min(15,10)=10→max(10,5)=10.
			name: "when panic and panic percent is small then scale up rate is limited",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              true,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 100),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     5,
				RecommendedInstances: 15,
			},
			expectedCorrected: 10,
		},
		{
			// A MinCorrectedForPanic value smaller than current lowers pastSample,
			// tightening the panic constraint further.
			// MinCorrected=6 → pastSample=min(6,10)=6; relConst=6+6=12;
			// corrected=min(30,12)=12→max(12,10)=12.
			name: "when panic and min corrected history is smaller then constraint tightens",
			args: CorrectedInstancesAlgorithm{
				IsPanic: true,
				History: func() *History {
					h := emptyHistory()
					h.MinCorrectedForPanic.Append(6)
					return h
				}(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 100),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     10,
				RecommendedInstances: 30,
			},
			expectedCorrected: 12,
		},
		{
			// Panic mode never scales down: corrected is always at least current.
			// pastSample=10, relConst=110; corrected=min(3,110)=3→max(3,10)=10.
			name: "when panic and recommended is below current then corrected is at least current",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              true,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         100,
				CurrentInstances:     10,
				RecommendedInstances: 3,
			},
			expectedCorrected: 10,
		},
		// ── global min/max bounds ─────────────────────────────────────────────
		{
			// When the corrected value falls below MinInstances it is raised to MinInstances.
			// stable equal path: corrected=5; min(max(5,8),20)=8.
			name: "when corrected is below min instances then return min instances",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         8,
				MaxInstances:         20,
				CurrentInstances:     5,
				RecommendedInstances: 5,
			},
			expectedCorrected: 8,
		},
		{
			// When the corrected value exceeds MaxInstances it is clamped to MaxInstances.
			// stable equal path: corrected=15; min(max(15,1),10)=10.
			name: "when corrected is above max instances then return max instances",
			args: CorrectedInstancesAlgorithm{
				IsPanic:              false,
				History:              emptyHistory(),
				Behavior:             makeBehavior(2, 20, v1alpha1.SelectPolicyOr, 2, 100, v1alpha1.SelectPolicyOr, 1000),
				MinInstances:         1,
				MaxInstances:         10,
				CurrentInstances:     15,
				RecommendedInstances: 15,
			},
			expectedCorrected: 10,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			corrected := tc.args.GetCorrectedInstances()
			assert.Equal(t, tc.expectedCorrected, corrected)
		})
	}
}
