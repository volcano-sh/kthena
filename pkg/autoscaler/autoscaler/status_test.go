/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
you may obtain a copy of the License at

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
	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func defaultBehavior() *v1alpha1.AutoscalingPolicyBehavior {
	return &v1alpha1.AutoscalingPolicyBehavior{
		ScaleDown: v1alpha1.AutoscalingPolicyStablePolicy{
			StabilizationWindow: &metav1.Duration{Duration: 1800 * time.Second},
			Instances:           ptr.To(int32(1)),
			Percent:             ptr.To(int32(100)),
		},
		ScaleUp: v1alpha1.AutoscalingPolicyScaleUpPolicy{
			StablePolicy: v1alpha1.AutoscalingPolicyStablePolicy{
				StabilizationWindow: &metav1.Duration{Duration: 1800 * time.Second},
				Instances:           ptr.To(int32(1)),
				Percent:             ptr.To(int32(100)),
			},
		},
	}
}

func TestCorrectedInstances_ScaleDown_BlockedByStabilizationWindow(t *testing.T) {
	behavior := defaultBehavior()
	status := NewStatus(behavior)
	currentInstances := int32(10)
	status.InitializeWithCurrentReplicas(currentInstances)

	corrected := algorithm.CorrectedInstancesAlgorithm{
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         int32(1),
		MaxInstances:         int32(10),
		CurrentInstances:     currentInstances,
		RecommendedInstances: 0,
	}.GetCorrectedInstances()
	assert.Equal(t, currentInstances, corrected, "stabilization window should block scale-down")
}

func TestCorrectedInstances_ScaleDown_ReinitializedWithCurrentReplicas(t *testing.T) {
	behavior := defaultBehavior()
	status := NewStatus(behavior)
	status.InitializeWithCurrentReplicas(1)
	status.AppendRecommendation(1)

	// Re-initialize when current instances change.
	status.InitializeWithCurrentReplicas(3)

	corrected := algorithm.CorrectedInstancesAlgorithm{
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         int32(1),
		MaxInstances:         int32(5),
		CurrentInstances:     int32(3),
		RecommendedInstances: 1,
	}.GetCorrectedInstances()
	assert.Equal(t, int32(3), corrected, "re-initialization with current replicas should block immediate scale-down")
}

func TestCorrectedInstances_ScaleDown_WindowExpires(t *testing.T) {
	behavior := &v1alpha1.AutoscalingPolicyBehavior{
		ScaleDown: v1alpha1.AutoscalingPolicyStablePolicy{
			StabilizationWindow: &metav1.Duration{Duration: 0},
			Instances:           ptr.To(int32(1)),
			Percent:             ptr.To(int32(100)),
		},
		ScaleUp: v1alpha1.AutoscalingPolicyScaleUpPolicy{
			StablePolicy: v1alpha1.AutoscalingPolicyStablePolicy{
				StabilizationWindow: &metav1.Duration{Duration: 0},
				Instances:           ptr.To(int32(1)),
				Percent:             ptr.To(int32(100)),
			},
		},
	}
	status := NewStatus(behavior)
	status.InitializeWithCurrentReplicas(3)

	corrected := algorithm.CorrectedInstancesAlgorithm{
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         int32(1),
		MaxInstances:         int32(5),
		CurrentInstances:     int32(3),
		RecommendedInstances: 1,
	}.GetCorrectedInstances()
	assert.Equal(t, int32(1), corrected, "scale-down should be allowed after stabilization window expires")
}

func TestCorrectedInstances_ScaleUp_BlockedByStabilizationWindow(t *testing.T) {
	behavior := defaultBehavior()
	status := NewStatus(behavior)
	currentInstances := int32(1)
	status.InitializeWithCurrentReplicas(currentInstances)

	corrected := algorithm.CorrectedInstancesAlgorithm{
		History:              status.History,
		Behavior:             behavior,
		MinInstances:         int32(1),
		MaxInstances:         int32(10),
		CurrentInstances:     currentInstances,
		RecommendedInstances: 10,
	}.GetCorrectedInstances()
	assert.Equal(t, currentInstances, corrected, "stabilization window should block scale-up")
}
