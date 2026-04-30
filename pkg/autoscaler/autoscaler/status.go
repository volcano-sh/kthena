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
	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	"github.com/volcano-sh/kthena/pkg/autoscaler/datastructure"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
)

type Status struct {
	PanicModeEndsAt           int64
	PanicModeHoldMilliseconds int64
	History                   *algorithm.History
}

func NewStatus(behavior *v1alpha1.AutoscalingPolicyBehavior) *Status {
	panicModeHoldMilliseconds := int64(0)
	if behavior.ScaleUp.PanicPolicy.PanicModeHold != nil {
		panicModeHoldMilliseconds = behavior.ScaleUp.PanicPolicy.PanicModeHold.Milliseconds()
	}
	scaleDownStabilizationWindowMilliseconds := int64(0)
	if behavior.ScaleDown.StabilizationWindow != nil {
		scaleDownStabilizationWindowMilliseconds = behavior.ScaleDown.StabilizationWindow.Milliseconds()
	}
	scaleUpStabilizationWindowMilliseconds := int64(0)
	if behavior.ScaleUp.StablePolicy.StabilizationWindow != nil {
		scaleUpStabilizationWindowMilliseconds = behavior.ScaleUp.StablePolicy.StabilizationWindow.Milliseconds()
	}
	scaleUpStablePolicyPeriodMilliseconds := int64(0)
	if behavior.ScaleUp.StablePolicy.Period != nil {
		scaleUpStablePolicyPeriodMilliseconds = behavior.ScaleUp.StablePolicy.Period.Milliseconds()
	}
	scaleDownPeriodMilliseconds := int64(0)
	if behavior.ScaleDown.Period != nil {
		scaleDownPeriodMilliseconds = behavior.ScaleDown.Period.Milliseconds()
	}
	return &Status{
		PanicModeEndsAt:           0,
		PanicModeHoldMilliseconds: panicModeHoldMilliseconds,
		History: &algorithm.History{
			MaxRecommendation:     datastructure.NewMaximumRecordSlidingWindow[int32](scaleDownStabilizationWindowMilliseconds),
			MinRecommendation:     datastructure.NewMinimumRecordSlidingWindow[int32](scaleUpStabilizationWindowMilliseconds),
			MaxCorrected:          datastructure.NewMaximumLineChartSlidingWindow[int32](scaleDownPeriodMilliseconds),
			MinCorrectedForStable: datastructure.NewMinimumLineChartSlidingWindow[int32](scaleUpStablePolicyPeriodMilliseconds),
			MinCorrectedForPanic:  datastructure.NewMinimumLineChartSlidingWindow[int32](behavior.ScaleUp.PanicPolicy.Period.Milliseconds()),
		},
	}
}

func (s *Status) AppendRecommendation(recommendedInstances int32) {
	s.History.MaxRecommendation.Append(recommendedInstances)
	s.History.MinRecommendation.Append(recommendedInstances)
}

func (s *Status) AppendCorrected(correctedInstances int32) {
	s.History.MaxCorrected.Append(correctedInstances)
	s.History.MinCorrectedForStable.Append(correctedInstances)
	s.History.MinCorrectedForPanic.Append(correctedInstances)
}

func (s *Status) RefreshPanicMode() {
	if s.PanicModeHoldMilliseconds == 0 {
		s.PanicModeEndsAt = 0
	} else {
		s.PanicModeEndsAt = util.GetCurrentTimestamp() + s.PanicModeHoldMilliseconds
	}
}

func (s *Status) IsPanicMode() bool {
	return s.PanicModeHoldMilliseconds > 0 && util.GetCurrentTimestamp() <= s.PanicModeEndsAt
}
