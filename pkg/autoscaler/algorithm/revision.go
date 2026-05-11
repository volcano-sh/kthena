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
	"math"

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/datastructure"
)

type CorrectedInstancesAlgorithm struct {
	IsPanic              bool
	History              *History
	Behavior             *v1alpha1.AutoscalingPolicyBehavior
	MinInstances         int32
	MaxInstances         int32
	CurrentInstances     int32
	RecommendedInstances int32
}

type History struct {
	MaxRecommendation     *datastructure.RmqRecordSlidingWindow[int32]
	MinRecommendation     *datastructure.RmqRecordSlidingWindow[int32]
	MaxCorrected          *datastructure.RmqLineChartSlidingWindow[int32]
	MinCorrectedForStable *datastructure.RmqLineChartSlidingWindow[int32]
	MinCorrectedForPanic  *datastructure.RmqLineChartSlidingWindow[int32]
}

func (alg CorrectedInstancesAlgorithm) GetCorrectedInstances() int32 {
	var corrected int32
	if alg.IsPanic {
		corrected = alg.getCorrectedInstancesForPanic()
	} else {
		corrected = alg.getCorrectedInstancesForStable()
	}
	return min(max(corrected, alg.MinInstances), alg.MaxInstances)
}

func (alg CorrectedInstancesAlgorithm) getCorrectedInstancesForPanic() int32 {
	corrected := alg.RecommendedInstances
	if pastSample, ok := alg.History.MinCorrectedForPanic.GetBest(alg.CurrentInstances); ok && pastSample > 0 {
		relativeConstraint := pastSample + int32(float64(pastSample)*float64(*alg.Behavior.ScaleUp.PanicPolicy.Percent)/100.0)
		corrected = min(corrected, relativeConstraint)
	}
	corrected = max(corrected, alg.CurrentInstances)
	return corrected
}

func (alg CorrectedInstancesAlgorithm) getCorrectedInstancesForStable() int32 {
	var corrected int32
	switch {
	case alg.RecommendedInstances < alg.CurrentInstances:
		corrected = alg.getCorrectedInstancesForStableScaleDown()
	case alg.RecommendedInstances > alg.CurrentInstances:
		corrected = alg.getCorrectedInstancesForStableScaleUp()
	default:
		corrected = alg.RecommendedInstances
	}
	return corrected
}

func (alg CorrectedInstancesAlgorithm) getCorrectedInstancesForStableScaleDown() int32 {
	corrected := alg.RecommendedInstances
	if betterRecommendation, ok := alg.History.MaxRecommendation.GetBest(); ok {
		corrected = max(corrected, betterRecommendation)
	}
	if pastSample, ok := alg.History.MaxCorrected.GetBest(alg.CurrentInstances); ok {
		absoluteConstraint := pastSample - *alg.Behavior.ScaleDown.Instances
		relativeConstraint := pastSample - pastSample*(*alg.Behavior.ScaleDown.Percent)/100
		var constraint int32
		switch alg.Behavior.ScaleDown.SelectPolicy {
		case v1alpha1.SelectPolicyOr:
			constraint = min(absoluteConstraint, relativeConstraint)
		case v1alpha1.SelectPolicyAnd:
			constraint = max(absoluteConstraint, relativeConstraint)
		default:
			constraint = math.MinInt32
		}
		corrected = max(corrected, constraint)
	}
	corrected = min(corrected, alg.CurrentInstances)
	return corrected
}

func (alg CorrectedInstancesAlgorithm) getCorrectedInstancesForStableScaleUp() int32 {
	corrected := alg.RecommendedInstances
	if betterRecommendation, ok := alg.History.MinRecommendation.GetBest(); ok {
		corrected = min(corrected, betterRecommendation)
	}
	if pastSample, ok := alg.History.MinCorrectedForStable.GetBest(alg.CurrentInstances); ok {
		absoluteConstraint := pastSample + *alg.Behavior.ScaleUp.StablePolicy.Instances
		relativeConstraint := pastSample + pastSample*(*alg.Behavior.ScaleUp.StablePolicy.Percent)/100
		var constraint int32
		switch alg.Behavior.ScaleUp.StablePolicy.SelectPolicy {
		case v1alpha1.SelectPolicyOr:
			constraint = max(absoluteConstraint, relativeConstraint)
		case v1alpha1.SelectPolicyAnd:
			constraint = min(absoluteConstraint, relativeConstraint)
		default:
			constraint = math.MaxInt32
		}
		corrected = min(corrected, constraint)
	}
	corrected = max(corrected, alg.CurrentInstances)
	return corrected
}
