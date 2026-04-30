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

// neverExpireMs must stay well below MaxInt64/2 because LineChartSlidingWindow
// internally computes maxDriftingMilliseconds = 2 * freshMilliseconds; overflow
// would cause drifting values to expire immediately.
const neverExpireMs = int64(1e15)

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
		{
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
		{
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
		{
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
		{
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
