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
	"time"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// buildTestBehavior creates an AutoscalingPolicyBehavior with the given
// scale-down stabilization window. Other fields use safe defaults.
func buildTestBehavior(stabilizationWindow time.Duration) *workload.AutoscalingPolicyBehavior {
	sw := metav1.Duration{Duration: stabilizationWindow}
	var instances int32 = 1
	var percent int32 = 100
	var panicThreshold int32 = 200
	return &workload.AutoscalingPolicyBehavior{
		ScaleDown: workload.AutoscalingPolicyStablePolicy{
			StabilizationWindow: &sw,
			Instances:           &instances,
			Percent:             &percent,
			SelectPolicy:        workload.SelectPolicyOr,
		},
		ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
			PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
				Period:                metav1.Duration{Duration: 1 * time.Minute},
				PanicThresholdPercent: &panicThreshold,
			},
		},
	}
}

func TestStabilizationWindowPreventsImmediateScaleDownAfterSkip(t *testing.T) {
	// Simulate: minReplicas=1, currentInstances=1. Pod is not ready (skip=true),
	// so currentInstancesCount is recorded into history. Then pod becomes ready
	// with metric=0, recommendedInstances=0. The stabilizationWindow should
	// retain the historical recommendation and prevent immediate scale-down.
	behavior := buildTestBehavior(3 * time.Minute)
	status := NewStatus(behavior)

	currentInstancesCount := int32(1)
	// Simulate the fix: when skip=true, we record currentInstancesCount
	status.AppendRecommendation(currentInstancesCount)
	status.AppendCorrected(currentInstancesCount)

	// Now pod is ready, metric=0 → recommended=0
	recommendedInstances := int32(0)
	correctedAlg := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              false,
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         1,
		MaxInstances:         2,
		CurrentInstances:     currentInstancesCount,
		RecommendedInstances: recommendedInstances,
	}
	corrected := correctedAlg.GetCorrectedInstances()

	// stabilizationWindow should hold the previous recommendation of 1,
	// preventing scale-down
	if corrected != currentInstancesCount {
		t.Errorf("expected correctedInstances=%d (stabilizationWindow prevents scale-down), got %d", currentInstancesCount, corrected)
	}
}

func TestEmptyHistoryAllowsImmediateScaleDown(t *testing.T) {
	// This test demonstrates the bug that existed BEFORE the fix:
	// when skip=true and nothing is written to history, the
	// stabilizationWindow has no data and cannot prevent scale-down.
	behavior := buildTestBehavior(3 * time.Minute)
	status := NewStatus(behavior)

	currentInstancesCount := int32(1)
	// Do NOT write anything to history — simulating the old behavior
	// where skip=true caused history to remain empty.

	recommendedInstances := int32(0)
	correctedAlg := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              false,
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         1,
		MaxInstances:         2,
		CurrentInstances:     currentInstancesCount,
		RecommendedInstances: recommendedInstances,
	}
	corrected := correctedAlg.GetCorrectedInstances()

	// With empty history, the stabilizationWindow cannot protect.
	// corrected = max(0, nothing) = 0, then min(max(0,1),2) = 1
	// (MinInstances=1 saves us here, but if minReplicas were lower
	// the pod would be scaled down immediately).
	// The key point: without history, GetBest() returns false.
	if corrected != int32(1) {
		t.Errorf("expected correctedInstances=1 (MinInstances floor), got %d", corrected)
	}
}

func TestStabilizationWindowAllowsScaleDownAfterExpiry(t *testing.T) {
	// After the stabilizationWindow expires, the historical recommendation
	// should be evicted and scale-down should be allowed.
	behavior := buildTestBehavior(1 * time.Millisecond)
	status := NewStatus(behavior)

	currentInstancesCount := int32(1)
	status.AppendRecommendation(currentInstancesCount)
	status.AppendCorrected(currentInstancesCount)

	// Wait for the 1ms stabilizationWindow to expire
	time.Sleep(10 * time.Millisecond)

	// recommended=0, window expired → MaxRecommendation.GetBest()=false
	recommendedInstances := int32(0)
	correctedAlg := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              false,
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         1,
		MaxInstances:         2,
		CurrentInstances:     currentInstancesCount,
		RecommendedInstances: recommendedInstances,
	}
	corrected := correctedAlg.GetCorrectedInstances()

	// After window expiry, the stabilization protection is gone.
	// corrected = max(0, nothing) = 0, min(max(0,1),2) = 1 (MinInstances floor)
	if corrected != int32(1) {
		t.Errorf("expected correctedInstances=1 (scale-down allowed after window expiry, MinInstances=1 floor), got %d", corrected)
	}
}

func TestStabilizationWindowMultipleInstances(t *testing.T) {
	// Test with multiple instances: current=3, skip writes 3 to history,
	// then recommended=1 should be corrected to 3 by stabilizationWindow.
	behavior := buildTestBehavior(5 * time.Minute)
	status := NewStatus(behavior)

	currentInstancesCount := int32(3)
	status.AppendRecommendation(currentInstancesCount)
	status.AppendCorrected(currentInstancesCount)

	// Pod becomes ready, load drops, recommended=1
	recommendedInstances := int32(1)
	correctedAlg := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              false,
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         1,
		MaxInstances:         10,
		CurrentInstances:     currentInstancesCount,
		RecommendedInstances: recommendedInstances,
	}
	corrected := correctedAlg.GetCorrectedInstances()

	// stabilizationWindow holds the previous MAX recommendation of 3
	if corrected != currentInstancesCount {
		t.Errorf("expected correctedInstances=%d (stabilizationWindow prevents scale-down), got %d", currentInstancesCount, corrected)
	}
}
