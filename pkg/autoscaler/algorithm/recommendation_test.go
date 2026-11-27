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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetRecommendedInstances(t *testing.T) {
	type TestCase struct {
		name                string
		args                RecommendedInstancesAlgorithm
		expectedRecommended int32
		expectedSkip        bool
	}

	testcases := []TestCase{
		{
			name: "givenInstancesLessThanMinInstances_thenReturnMinInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(5),
				MaxInstances:                  int32(10),
				CurrentInstancesCount:         int32(4),
				Tolerance:                     0.1,
				MetricTargets:                 Metrics{},
				UnreadyInstancesCountByMetric: sameUnready(4),
				ReadyInstancesMetrics:         samePods([]Metrics{}),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(5),
			expectedSkip:        false,
		},
		{
			name: "givenInstancesGreaterThanMaxInstances_thenReturnMaxInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(5),
				MaxInstances:                  int32(10),
				CurrentInstancesCount:         int32(11),
				Tolerance:                     0.1,
				MetricTargets:                 Metrics{},
				UnreadyInstancesCountByMetric: sameUnready(0),
				ReadyInstancesMetrics:         samePods([]Metrics{}),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(10),
			expectedSkip:        false,
		},
		{
			name: "givenNoAvailableMetrics_thenSkip",
			args: RecommendedInstancesAlgorithm{
				MinInstances:          int32(5),
				MaxInstances:          int32(10),
				CurrentInstancesCount: int32(7),
				Tolerance:             0.1,
				MetricTargets: Metrics{
					"a": 0.5,
					"b": 8.0,
				},
				UnreadyInstancesCountByMetric: sameUnready(7, "a", "b"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a", "b"),
				ExternalMetrics:               Metrics{},
			},
			expectedSkip: true,
		},
		{
			name: "givenMultipleMetrics_thenReturnMaximumRecommendation",
			args: RecommendedInstancesAlgorithm{
				MinInstances:          int32(1),
				MaxInstances:          int32(1000000),
				CurrentInstancesCount: int32(3),
				Tolerance:             0.0,
				MetricTargets: Metrics{
					"a": 3.0,
					"b": 5.0,
					"c": 4.0,
				},
				UnreadyInstancesCountByMetric: sameUnready(0, "a", "b", "c"),
				ReadyInstancesMetrics: samePods(slices.Repeat([]Metrics{{
					"a": 6.0,
					"b": 500.0,
					"c": 20.0,
				}}, 3), "a", "b", "c"),
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(300),
			expectedSkip:        false,
		},
		{
			name: "givenTargetWithZeroValue_thenReturnMaximumInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(1),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 0.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{{"a": 1.0}}, "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(100),
			expectedSkip:        false,
		},
		{
			name: "givenTargetWithExtremelySmallValue_thenReturnMaximumInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(1),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1e-100},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{{"a": 1e100}}, "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(100),
			expectedSkip:        false,
		},
		{
			name: "givenReadyInstances_shouldCalculateAverage",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(3),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics: samePods([]Metrics{
					{"a": 19.1},
					{"a": 3.1},
					{"a": 7.1},
				}, "a"),
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(30),
			expectedSkip:        false,
		},
		{
			name: "whenDesiredIsLessThanMinInstances_thenReturnMinInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(5),
				MaxInstances:                  int32(20),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 0.1}}, 10), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(5),
			expectedSkip:        false,
		},
		{
			name: "whenDesiredIsGreaterThanMaxInstances_thenReturnMaxInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(5),
				MaxInstances:                  int32(20),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 3.0}}, 10), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(20),
			expectedSkip:        false,
		},
		{
			name: "givenNoUnreadyInstancesAndNoMissingInstances_whenWithinLowestTolerance_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(9),
				Tolerance:                     0.5,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 0.51}}, 10), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(9),
			expectedSkip:        false,
		},
		{
			name: "givenNoUnreadyInstancesAndNoMissingInstances_whenOutOfLowestTolerance_thenReturnDesired",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(9),
				Tolerance:                     0.5,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 0.49}}, 10), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(5),
			expectedSkip:        false,
		},
		{
			name: "givenNoUnreadyInstancesAndNoMissingInstances_whenWithinHighestTolerance_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(9),
				Tolerance:                     0.5,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 1.49}}, 10), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(9),
			expectedSkip:        false,
		},
		{
			name: "givenNoUnreadyInstancesAndNoMissingInstances_whenOutOfHighestTolerance_thenReturnDesired",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(9),
				Tolerance:                     0.5,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 1.51}}, 10), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(16),
			expectedSkip:        false,
		},
		{
			name: "givenUnreadyInstances_whenShouldScaleDown_thenIgnoreUnreadyInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(58),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(50, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 0.15}}, 8), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(2),
			expectedSkip:        false,
		},
		{
			name: "givenUnreadyInstances_whenShouldScaleUp_thenThreatUnreadyInstancesAsZero",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(18),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(10, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 3.9}}, 8), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(32),
			expectedSkip:        false,
		},
		{
			name: "givenTooManyUnreadyInstances_whenEstimatedResultIsOpposite_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(58),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(50, "a"),
				ReadyInstancesMetrics:         samePods(slices.Repeat([]Metrics{{"a": 3.9}}, 8), "a"),
				ExternalMetrics:               Metrics{},
			},
			expectedRecommended: int32(58),
			expectedSkip:        false,
		},
		{
			name: "givenMissingInstances_whenShouldScaleDown_thenTreatMissingInstancesAsTarget",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics: samePods(slices.Concat(
					slices.Repeat([]Metrics{{}}, 2),
					slices.Repeat([]Metrics{{"a": 0.5}}, 8),
				), "a"),
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(6),
			expectedSkip:        false,
		},
		{
			name: "givenMissingInstances_whenShouldScaleUp_thenTreatMissingInstancesAsZero",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics: samePods(slices.Concat(
					slices.Repeat([]Metrics{{}}, 2),
					slices.Repeat([]Metrics{{"a": 2.9}}, 8),
				), "a"),
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(24),
			expectedSkip:        false,
		},
		{
			name: "givenTooManyMissingInstances_whenEstimatedResultIsOpposite_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(58),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics: samePods(slices.Concat(
					slices.Repeat([]Metrics{{}}, 50),
					slices.Repeat([]Metrics{{"a": 2.9}}, 8),
				), "a"),
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(58),
			expectedSkip:        false,
		},
		{
			name: "givenTooManyNonExistingInstances_whenEstimatedResultIsOpposite_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(58),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics: samePods(slices.Concat(
					slices.Repeat([]Metrics{{}}, 4),
					slices.Repeat([]Metrics{{"a": 2.9}}, 8),
				), "a"),
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(58),
			expectedSkip:        false,
		},
		{
			name: "givenExternalMetric_whenWithinLowestTolerance_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.51,
				MetricTargets:                 Metrics{"a": 3.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a"),
				ExternalMetrics:               Metrics{"a": 14.9},
			},
			expectedRecommended: int32(10),
			expectedSkip:        false,
		},
		{
			name: "givenExternalMetric_whenOutOfLowestTolerance_thenReturnDesired",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.49,
				MetricTargets:                 Metrics{"a": 3.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a"),
				ExternalMetrics:               Metrics{"a": 14.9},
			},
			expectedRecommended: int32(5),
			expectedSkip:        false,
		},
		{
			name: "givenExternalMetric_whenWithinHighestTolerance_thenReturnCurrent",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.51,
				MetricTargets:                 Metrics{"a": 3.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a"),
				ExternalMetrics:               Metrics{"a": 44.9},
			},
			expectedRecommended: int32(10),
			expectedSkip:        false,
		},
		{
			name: "givenExternalMetric_whenOutOfHighestTolerance_thenReturnDesired",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(10),
				Tolerance:                     0.49,
				MetricTargets:                 Metrics{"a": 3.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a"),
				ExternalMetrics:               Metrics{"a": 44.9},
			},
			expectedRecommended: int32(15),
			expectedSkip:        false,
		},
		{
			name: "givenExternalTargetWithZeroValue_thenReturnMaximumInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(1),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 0.0},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a"),
				ExternalMetrics:               Metrics{"a": 1.0},
			},
			expectedRecommended: int32(100),
			expectedSkip:        false,
		},
		{
			name: "givenExternalTargetWithExtremelySmallValue_thenReturnMaximumInstances",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(100),
				CurrentInstancesCount:         int32(1),
				Tolerance:                     0.0,
				MetricTargets:                 Metrics{"a": 1e-100},
				UnreadyInstancesCountByMetric: sameUnready(0, "a"),
				ReadyInstancesMetrics:         samePods([]Metrics{}, "a"),
				ExternalMetrics:               Metrics{"a": 1e100},
			},
			expectedRecommended: int32(100),
			expectedSkip:        false,
		},
		{
			name: "givenMetricsReportedByDisjointPods_thenOnlyEachMetricsPodsCount",
			args: RecommendedInstancesAlgorithm{
				MinInstances:                  int32(1),
				MaxInstances:                  int32(10),
				CurrentInstancesCount:         int32(4),
				Tolerance:                     0.1,
				MetricTargets:                 Metrics{"waiting": 10.0, "kv": 0.8},
				UnreadyInstancesCountByMetric: map[string]int32{"waiting": 0, "kv": 0},
				ReadyInstancesMetrics: map[string][]Metrics{
					"waiting": slices.Repeat([]Metrics{{"waiting": 0.0}}, 2),
					"kv":      slices.Repeat([]Metrics{{"kv": 0.0}}, 2),
				},
				ExternalMetrics: Metrics{},
			},
			expectedRecommended: int32(1),
			expectedSkip:        false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			recommended, skip := tc.args.GetRecommendedInstances()
			assert.Equal(tc.expectedRecommended, recommended)
			assert.Equal(tc.expectedSkip, skip)
		})
	}
}

// samePods gives every listed metric the same pod entries: the shape the
// collector returns when all metrics are scraped from the same pods.
func samePods(entries []Metrics, names ...string) map[string][]Metrics {
	out := make(map[string][]Metrics, len(names))
	for _, name := range names {
		out[name] = entries
	}
	return out
}

// sameUnready gives every listed metric the same unready pod count.
func sameUnready(count int32, names ...string) map[string]int32 {
	out := make(map[string]int32, len(names))
	for _, name := range names {
		out[name] = count
	}
	return out
}

func TestGetRecommendedInstancesWithReplicaCount(t *testing.T) {
	entryPods := func(count int, value float64) []Metrics {
		return slices.Repeat([]Metrics{{"queue_depth": value}}, count)
	}
	replicasOf := func(ready, all float64) map[string]ReplicaCount {
		return map[string]ReplicaCount{"queue_depth": {Ready: ready, All: all}}
	}

	testcases := []struct {
		name     string
		current  int32
		pods     []Metrics
		unready  int32
		replicas map[string]ReplicaCount
		expected int32
	}{
		{name: "scale up: 1.5 x 2 replicas", current: 2, pods: entryPods(4, 15), replicas: replicasOf(2, 2), expected: 3},
		{name: "scale down: 0.5 x 4 replicas", current: 4, pods: entryPods(8, 5), replicas: replicasOf(4, 4), expected: 2},
		{name: "fractional replicas: 1.5 x 1.5 replicas", current: 2, pods: entryPods(3, 15), replicas: replicasOf(1.5, 1.5), expected: 3},
		{
			// 1.5 x 2 replicas = 3 is the load measured on the pods that exist, so it can be
			// below the current count when some replicas have no pods yet.
			name: "first path can go below the current count", current: 4, pods: entryPods(2, 15), replicas: replicasOf(2, 2), expected: 3,
		},
		{
			// A missing pod counts as zero load on a scale up: 45 / 4 pods / 10 = 1.125.
			name: "pod missing its metric on scale up: 1.125 x 2 replicas", current: 2, pods: append(entryPods(3, 15), Metrics{}), replicas: replicasOf(2, 2), expected: 3,
		},
		{
			// A missing pod counts as at target on a scale down: (15 + 10) / 4 pods / 10 = 0.625.
			name: "pod missing its metric on scale down: 0.625 x 2 replicas", current: 4, pods: append(entryPods(3, 5), Metrics{}), replicas: replicasOf(2, 2), expected: 2,
		},
		{name: "missing pods bring the ratio within tolerance", current: 2, pods: append(entryPods(3, 14), Metrics{}), replicas: replicasOf(2, 2), expected: 2},
		{name: "missing pods reverse the direction", current: 2, pods: append(entryPods(2, 12), Metrics{}, Metrics{}), replicas: replicasOf(2, 2), expected: 2},
		{
			// 120 / 6 pods / 10 = 2, and scaling up counts the replicas of unready pods too.
			name: "unready pods on scale up use All: 2 x 3 replicas", current: 3, pods: entryPods(4, 30), unready: 2, replicas: replicasOf(2, 3), expected: 6,
		},
		{
			// Unready pods are left out of a scale down, so their replicas are too.
			name: "unready pods on scale down use Ready: 0.5 x 3 replicas", current: 4, pods: entryPods(6, 5), unready: 2, replicas: replicasOf(3, 4), expected: 2,
		},
		{
			// 25 / 5 pods / 10 = 0.5 is a scale down, so the unready pods are left out, and the
			// missing pod counts as at target: (25 + 10) / 6 pods / 10 = 0.583.
			name: "missing and unready pods on scale down use Ready: 0.583 x 3 replicas", current: 4, pods: append(entryPods(5, 5), Metrics{}), unready: 2, replicas: replicasOf(3, 4), expected: 2,
		},
		{
			// 120 / 4 pods / 10 = 3, and 3 x 0.5 replicas = 2 would be below the current 3.
			name: "second path never scales down under load", current: 3, pods: append(entryPods(3, 40), Metrics{}), replicas: replicasOf(0.5, 0.5), expected: 3,
		},
		{name: "no entry counts each pod as one replica", current: 2, pods: entryPods(4, 15), expected: 6},
		{name: "another metric's entry doesn't apply", current: 2, pods: entryPods(4, 15), replicas: map[string]ReplicaCount{"other": {Ready: 2, All: 2}}, expected: 6},
		{name: "a zero entry counts each pod as one replica", current: 2, pods: entryPods(4, 15), replicas: replicasOf(0, 0), expected: 6},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			alg := RecommendedInstancesAlgorithm{
				MinInstances:                  1,
				MaxInstances:                  100,
				CurrentInstancesCount:         tc.current,
				Tolerance:                     0.1,
				MetricTargets:                 Metrics{"queue_depth": 10},
				UnreadyInstancesCountByMetric: sameUnready(tc.unready, "queue_depth"),
				ReadyInstancesMetrics:         samePods(tc.pods, "queue_depth"),
				ExternalMetrics:               Metrics{},
				ReplicasByMetric:              tc.replicas,
			}

			recommended, skip := alg.GetRecommendedInstances()

			assert.False(t, skip)
			assert.Equal(t, tc.expected, recommended)
		})
	}
}

func TestExternalMetricIgnoresReplicaCount(t *testing.T) {
	alg := RecommendedInstancesAlgorithm{
		MinInstances:          1,
		MaxInstances:          100,
		CurrentInstancesCount: 2,
		Tolerance:             0.1,
		MetricTargets:         Metrics{"queue_depth": 10},
		ExternalMetrics:       Metrics{"queue_depth": 30},
		ReplicasByMetric:      map[string]ReplicaCount{"queue_depth": {Ready: 2, All: 2}},
	}

	recommended, skip := alg.GetRecommendedInstances()

	assert.False(t, skip)
	assert.Equal(t, int32(3), recommended)
}
