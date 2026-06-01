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

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func int32Ptr(v int32) *int32 { return &v }

func durationPtr(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

// TestNewStatusWiresMaxCorrectedAsMaximumWindow guards against a regression
// where History.MaxCorrected was previously created via
// NewMinimumLineChartSlidingWindow. Min/Max windows share the same Append
// signature, so the bug was invisible to the algorithm-level tests; this test
// exercises the Status wiring directly by appending samples and asserting the
// window returns the maximum value via GetBest.
func TestNewStatusWiresMaxCorrectedAsMaximumWindow(t *testing.T) {
	behavior := &v1alpha1.AutoscalingPolicyBehavior{
		ScaleDown: v1alpha1.AutoscalingPolicyStablePolicy{
			Period: durationPtr(time.Hour),
		},
		ScaleUp: v1alpha1.AutoscalingPolicyScaleUpPolicy{
			StablePolicy: v1alpha1.AutoscalingPolicyStablePolicy{
				Period: durationPtr(time.Hour),
			},
			PanicPolicy: v1alpha1.AutoscalingPolicyPanicPolicy{
				Percent: int32Ptr(1000),
				Period:  metav1.Duration{Duration: time.Hour},
			},
		},
	}

	status := NewStatus(behavior)
	for _, v := range []int32{3, 8, 5} {
		status.History.MaxCorrected.Append(v)
	}

	// currentValue=4 lies between the min sample (3) and max sample (8), so a
	// Maximum window returns 8 while a Minimum window would return 3.
	got, ok := status.History.MaxCorrected.GetBest(4)
	assert.True(t, ok)
	assert.Equal(t, int32(8), got, "MaxCorrected must return the maximum sample; a Minimum window would return 3")
}
