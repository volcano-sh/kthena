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

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
)

// A new Status has an empty history. This happens after a controller restart,
// a policy edit and a new policy. Both stabilization windows must still start
// from the current replicas, as Kubernetes HPA does.
func TestScaleOneTargetHoldsStabilizationWindowsWithEmptyHistory(t *testing.T) {
	tests := []struct {
		name            string
		current         int32
		loadPerPod      float64
		downWindow      time.Duration
		upWindow        time.Duration
		reconciles      int
		waitBeforeLast  time.Duration
		wantRecommended int32
		wantCorrected   int32
	}{
		{name: "scale-down window keeps the current replicas on the first reconcile", current: 4, loadPerPod: 0, downWindow: time.Hour, reconciles: 1, wantRecommended: 1, wantCorrected: 4},
		{name: "scale-down window keeps the current replicas on later reconciles", current: 4, loadPerPod: 0, downWindow: time.Hour, reconciles: 3, wantRecommended: 1, wantCorrected: 4},
		{name: "scales down once the scale-down window has passed", current: 4, loadPerPod: 0, downWindow: 50 * time.Millisecond, reconciles: 2, waitBeforeLast: 200 * time.Millisecond, wantRecommended: 1, wantCorrected: 1},
		{name: "no scale-down window scales down at once", current: 4, loadPerPod: 0, reconciles: 1, wantRecommended: 1, wantCorrected: 1},
		{name: "scale-up window keeps the current replicas on the first reconcile", current: 2, loadPerPod: 20, upWindow: time.Hour, reconciles: 1, wantRecommended: 4, wantCorrected: 2},
		{name: "scales up once the scale-up window has passed", current: 2, loadPerPod: 20, upWindow: 50 * time.Millisecond, reconciles: 2, waitBeforeLast: 200 * time.Millisecond, wantRecommended: 4, wantCorrected: 4},
		{name: "no scale-up window scales up at once", current: 2, loadPerPod: 20, reconciles: 1, wantRecommended: 4, wantCorrected: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			behavior := &workload.AutoscalingPolicyBehavior{
				ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
					StablePolicy: workload.AutoscalingPolicyStablePolicy{
						Instances:           ptr(int32(1000)),
						Percent:             ptr(int32(1000)),
						Period:              &metav1.Duration{Duration: 15 * time.Second},
						SelectPolicy:        workload.SelectPolicyOr,
						StabilizationWindow: &metav1.Duration{Duration: tt.upWindow},
					},
					PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
						Percent: ptr(int32(1000)),
						Period:  metav1.Duration{Duration: 15 * time.Second},
					},
				},
				ScaleDown: workload.AutoscalingPolicyStablePolicy{
					Instances:           ptr(int32(1000)),
					Percent:             ptr(int32(100)),
					Period:              &metav1.Duration{Duration: 15 * time.Second},
					SelectPolicy:        workload.SelectPolicyOr,
					StabilizationWindow: &metav1.Duration{Duration: tt.downWindow},
				},
			}
			status := NewStatus(behavior)
			pods := make([]algorithm.Metrics, tt.current)
			for i := range pods {
				pods[i] = algorithm.Metrics{"load": tt.loadPerPod}
			}

			var result scaleOneTargetResult
			for i := range tt.reconciles {
				if i == tt.reconciles-1 {
					time.Sleep(tt.waitBeforeLast)
				}
				result = scaleOneTarget(scaleOneTargetInput{
					Status:                status,
					Behavior:              behavior,
					MinReplicas:           1,
					MaxReplicas:           4,
					CurrentReplicas:       tt.current,
					TolerancePercent:      10,
					MetricTargets:         algorithm.Metrics{"load": 10},
					ReadyInstancesMetrics: map[string][]algorithm.Metrics{"load": pods},
				})
				recordScaleOneTargetResult(status, result)
			}

			assert.False(t, result.Skip)
			assert.Equal(t, tt.wantRecommended, result.RecommendedReplicas)
			assert.Equal(t, tt.wantCorrected, result.CorrectedReplicas)
		})
	}
}
