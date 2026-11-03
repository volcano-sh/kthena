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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The scenarios in this file run through Scale and Optimize with real pod labels and a
// real metrics endpoint. The target value is always 10 and the tolerance is 10%, so a pod
// reporting 15 gives a load ratio of 15 / 10 = 1.5. The expected results follow the
// formula in each scenario name: load ratio x replicas behind the selected pods.
const (
	replicaUnitsMetric = "queue_depth"
	replicaUnitsTarget = 10
	skippedScaling     = int32(-1)
)

type podState int

const (
	podReady podState = iota
	podUnready
	podTerminating
)

// podSet describes pods of one ServingGroup and role.
type podSet struct {
	modelServing string
	group        int
	role         string
	count        int
	state        podState
	worker       bool
	labels       map[string]string
}

// metricsPort starts a test server that plays the part of every pod's metrics endpoint,
// and returns its port.
func metricsPort(t *testing.T, payload string) int32 {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err)
	return int32(port)
}

// newScenarioPods returns a pod lister with the given pods, and the port of a metrics
// endpoint that reports the given load for every pod.
func newScenarioPods(t *testing.T, load float64, sets []podSet) (corelister.PodLister, int32) {
	t.Helper()
	port := metricsPort(t, fmt.Sprintf("# TYPE %s gauge\n%s %g\n", replicaUnitsMetric, replicaUnitsMetric, load))
	return newPodLister(t, sets), port
}

func newPodLister(t *testing.T, sets []podSet) corelister.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for setIndex, set := range sets {
		for i := range set.count {
			require.NoError(t, indexer.Add(newScenarioPod(set, setIndex, i)))
		}
	}
	return corelister.NewPodLister(indexer)
}

func newScenarioPod(set podSet, setIndex, roleIndex int) *corev1.Pod {
	groupName := fmt.Sprintf("%s-%d", set.modelServing, set.group)
	labels := map[string]string{
		workload.ModelServingNameLabelKey: set.modelServing,
		workload.GroupNameLabelKey:        groupName,
		workload.RoleLabelKey:             set.role,
		workload.RoleIDKey:                fmt.Sprintf("%s-%d", set.role, roleIndex),
		workload.RevisionLabelKey:         "revision-1",
	}
	if !set.worker {
		labels[workload.EntryLabelKey] = "true"
	}
	for key, value := range set.labels {
		labels[key] = value
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d-%d", groupName, setIndex, roleIndex), Namespace: "default", Labels: labels},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "127.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	switch set.state {
	case podUnready:
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
	case podTerminating:
		now := metav1.Now()
		pod.DeletionTimestamp = &now
	}
	return pod
}

// groups returns one podSet per ServingGroup, with the given number of pods in each. For
// example, groups("model", "server", 2, 1, 1) describes 3 groups holding 2, 1 and 1 pods.
func groups(modelServing, role string, podsPerGroup ...int) []podSet {
	sets := make([]podSet, 0, len(podsPerGroup))
	for group, count := range podsPerGroup {
		sets = append(sets, podSet{modelServing: modelServing, group: group, role: role, count: count})
	}
	return sets
}

func withLabels(sets []podSet, labels map[string]string) []podSet {
	for i := range sets {
		sets[i].labels = labels
	}
	return sets
}

func scenarioPolicySpec() workload.AutoscalingPolicySpec {
	period := &metav1.Duration{Duration: 15 * time.Second}
	return workload.AutoscalingPolicySpec{
		TolerancePercent: 10,
		Metrics:          []workload.AutoscalingPolicyMetric{{Name: replicaUnitsMetric, TargetValue: *resource.NewQuantity(replicaUnitsTarget, resource.DecimalSI)}},
		Behavior: workload.AutoscalingPolicyBehavior{
			ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
				StablePolicy: workload.AutoscalingPolicyStablePolicy{
					Instances:    ptr(int32(1000)),
					Percent:      ptr(int32(1000)),
					Period:       period,
					SelectPolicy: workload.SelectPolicyOr,
				},
				PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
					Percent:               ptr(int32(1000)),
					Period:                *period,
					PanicThresholdPercent: ptr(int32(1000)),
				},
			},
			ScaleDown: workload.AutoscalingPolicyStablePolicy{
				Instances:           ptr(int32(1000)),
				Percent:             ptr(int32(100)),
				Period:              period,
				SelectPolicy:        workload.SelectPolicyOr,
				StabilizationWindow: &metav1.Duration{},
			},
		},
	}
}

func podSources(port int32, selector *metav1.LabelSelector) map[string]workload.MetricSource {
	return map[string]workload.MetricSource{replicaUnitsMetric: {Pod: &workload.PodMetricSource{Name: replicaUnitsMetric, Port: port, LabelSelector: selector}}}
}

func matchLabels(key, value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
}

func modelServingRef(name string) corev1.ObjectReference {
	return corev1.ObjectReference{Kind: "ModelServing", Name: name}
}

func newPolicy(spec workload.AutoscalingPolicySpec) *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "default"}, Spec: spec}
}

func TestHomogeneousScenarios(t *testing.T) {
	prefillDecode := func() []podSet {
		return append(groups("model", "prefill", 1, 1), groups("model", "decode", 2, 2)...)
	}

	testcases := []struct {
		name     string
		replicas int32
		pods     []podSet
		load     float64
		selector *metav1.LabelSelector
		min, max int32
		want     int32
	}{
		// Several entry pods per ServingGroup, all running.
		{name: "scale up: 1.5 x 2 groups", replicas: 2, pods: groups("model", "server", 2, 2), load: 15, want: 3},
		{name: "scale down: 0.5 x 4 groups", replicas: 4, pods: groups("model", "server", 2, 2, 2, 2), load: 5, want: 2},
		{name: "within tolerance keeps replicas", replicas: 2, pods: groups("model", "server", 2, 2), load: 10.5, want: 2},
		{name: "load at target keeps replicas", replicas: 2, pods: groups("model", "server", 2, 2), load: 10, want: 2},
		{name: "four entry pods per group: 1.5 x 2 groups", replicas: 2, pods: groups("model", "server", 4, 4), load: 15, want: 3},
		{name: "two roles count once per group: 1.5 x 2 groups", replicas: 2, pods: prefillDecode(), load: 15, want: 3},

		// One entry pod per ServingGroup gives the same result as counting pods.
		{name: "one entry pod: 1.5 x 2 groups", replicas: 2, pods: groups("model", "server", 1, 1), load: 15, want: 3},
		{name: "one entry pod, extra group during a rollout: 1.5 x 4 groups", replicas: 2, pods: groups("model", "server", 1, 1, 1, 1), load: 15, want: 6},
		{name: "one entry pod, new replica has no pods yet: 1.5 x 2 groups", replicas: 3, pods: groups("model", "server", 1, 1), load: 15, want: 3},
		{name: "above target with replicas missing: 1.5 x 2 groups", replicas: 4, pods: groups("model", "server", 1, 1), load: 15, want: 3},
		{name: "below target with replicas missing: 0.5 x 2 groups", replicas: 4, pods: groups("model", "server", 1, 1), load: 5, want: 1},

		// ServingGroups only partly created. A group counts as a whole group, so the
		// last two results are higher than the 3 needed until the remaining pods exist.
		{name: "third group has no pods yet: 1.5 x 2 groups", replicas: 3, pods: groups("model", "server", 2, 2), load: 15, want: 3},
		{name: "half-created groups count as whole: 1.5 x 3 groups", replicas: 3, pods: groups("model", "server", 2, 1, 1), load: 15, want: 5},
		{name: "every group half-created: 1.5 x 3 groups", replicas: 3, pods: groups("model", "server", 1, 1, 1), load: 15, want: 5},

		// Unready pods.
		{
			name: "scale up counts groups with only unready pods: 120 / 6 pods / 10 x 3 groups", replicas: 3, load: 30, want: 6,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "server", count: 2},
				{modelServing: "model", group: 1, role: "server", count: 2},
				{modelServing: "model", group: 2, role: "server", count: 2, state: podUnready},
			},
		},
		{
			name: "scale down leaves out groups with only unready pods: 0.5 x 3 groups", replicas: 4, load: 5, want: 2,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "server", count: 2},
				{modelServing: "model", group: 1, role: "server", count: 2},
				{modelServing: "model", group: 2, role: "server", count: 2},
				{modelServing: "model", group: 3, role: "server", count: 2, state: podUnready},
			},
		},
		{
			name: "unready pods bring the ratio within tolerance: 42 / 4 pods / 10 = 1.05", replicas: 2, load: 14, want: 2,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "server", count: 2},
				{modelServing: "model", group: 1, role: "server", count: 1},
				{modelServing: "model", group: 1, role: "server", count: 1, state: podUnready},
			},
		},
		{
			name: "unready pods reverse the direction: 24 / 4 pods / 10 = 0.6", replicas: 2, load: 12, want: 2,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "server", count: 2},
				{modelServing: "model", group: 1, role: "server", count: 2, state: podUnready},
			},
		},
		{
			name: "unready pods reduce the scale up: 45 / 4 pods / 10 x 2 groups", replicas: 2, load: 15, want: 3,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "server", count: 2},
				{modelServing: "model", group: 1, role: "server", count: 1},
				{modelServing: "model", group: 1, role: "server", count: 1, state: podUnready},
			},
		},

		// Scaling is skipped, the same as before.
		{
			name: "a pod shutting down skips scaling", replicas: 2, load: 15, want: skippedScaling,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "server", count: 2},
				{modelServing: "model", group: 1, role: "server", count: 2, state: podTerminating},
			},
		},
		{name: "no pods skips scaling", replicas: 2, load: 15, want: skippedScaling},

		// Worker pods aren't entry pods.
		{
			name: "worker pods aren't counted: 1.5 x 2 groups", replicas: 2, load: 15, want: 3,
			pods: append(groups("model", "server", 2, 2),
				podSet{modelServing: "model", group: 0, role: "server", count: 6, worker: true},
				podSet{modelServing: "model", group: 1, role: "server", count: 6, worker: true}),
		},

		// Selectors that pick pods from every ServingGroup.
		{name: "selector by role: 1.5 x 2 groups", replicas: 2, pods: prefillDecode(), load: 15, selector: matchLabels(workload.RoleLabelKey, "decode"), want: 3},
		{name: "selector on a pod label: 1.5 x 2 groups", replicas: 2, pods: withLabels(groups("model", "server", 2, 2), map[string]string{"tlsMode": "istio"}), load: 15, selector: matchLabels("tlsMode", "istio"), want: 3},
		{name: "selector on the revision every group has: 1.5 x 2 groups", replicas: 2, pods: groups("model", "server", 2, 2), load: 15, selector: matchLabels(workload.RevisionLabelKey, "revision-1"), want: 3},
		{name: "selector on role-id: 1.5 x 2 groups", replicas: 2, pods: groups("model", "server", 2, 2), load: 15, selector: matchLabels(workload.RoleIDKey, "server-0"), want: 3},
		{
			name: "selector excluding a role-id: 3 x 3 groups", replicas: 3, pods: groups("model", "server", 2, 2, 2), load: 30, want: 9,
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: workload.RoleIDKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"server-0"}},
			}},
		},
		{
			name: "selector over two roles: 1.5 x 2 groups", replicas: 2, pods: prefillDecode(), load: 15, want: 3,
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: workload.RoleLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{"prefill", "decode"}},
			}},
		},

		// A selector on only some ServingGroups hides the others, so the result is too
		// low. The user guide warns against these selectors.
		{name: "selector on group-name counts one group: 1.5 x 1 group", replicas: 3, pods: groups("model", "server", 2, 2, 2), load: 15, selector: matchLabels(workload.GroupNameLabelKey, "model-0"), want: 2},
		{
			name: "selector on a revision only some groups have: 1.5 x 2 groups", replicas: 4, load: 15, want: 3,
			selector: matchLabels(workload.RevisionLabelKey, "revision-1"),
			pods: append(groups("model", "server", 2, 2),
				podSet{modelServing: "model", group: 2, role: "server", count: 2, labels: map[string]string{workload.RevisionLabelKey: "revision-2"}},
				podSet{modelServing: "model", group: 3, role: "server", count: 2, labels: map[string]string{workload.RevisionLabelKey: "revision-2"}}),
		},

		// Replica limits.
		{name: "maxReplicas limits the result", replicas: 2, pods: groups("model", "server", 2, 2), load: 100, max: 4, want: 4},
		{name: "minReplicas limits the result", replicas: 4, pods: groups("model", "server", 2, 2, 2, 2), load: 1, min: 2, want: 2},
		{name: "below minReplicas goes to minReplicas", replicas: 1, pods: groups("model", "server", 2), load: 1, min: 3, want: 3},
		{name: "above maxReplicas goes to maxReplicas", replicas: 9, pods: groups("model", "server", 2, 2), load: 15, max: 5, want: 5},

		// Zero replicas.
		{name: "zero replicas, pods not deleted yet: 1.5 x 1 group", replicas: 0, pods: groups("model", "server", 2), load: 15, want: 2},
		{name: "zero replicas, no pods skips scaling", replicas: 0, load: 15, want: skippedScaling},
		{name: "zero replicas with minReplicas 1 goes to 1", replicas: 0, load: 15, min: 1, want: 1},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			podLister, port := newScenarioPods(t, tc.load, tc.pods)
			maxReplicas := tc.max
			if maxReplicas == 0 {
				maxReplicas = 100
			}
			spec := scenarioPolicySpec()
			spec.HomogeneousTarget = &workload.HomogeneousTarget{
				Target:      workload.Target{TargetRef: modelServingRef("model"), MetricSources: podSources(port, tc.selector)},
				MinReplicas: tc.min,
				MaxReplicas: maxReplicas,
			}
			policy := newPolicy(spec)

			got, err := NewAutoscaler(policy).Scale(context.Background(), podLister, policy, tc.replicas)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHomogeneousScenarioUsesLargestResultAcrossMetrics(t *testing.T) {
	port := metricsPort(t, "# TYPE decode_queue gauge\ndecode_queue 15\n# TYPE prefill_queue gauge\nprefill_queue 30\n")
	podLister := newPodLister(t, append(groups("model", "prefill", 1, 1), groups("model", "decode", 2, 2)...))
	spec := scenarioPolicySpec()
	spec.Metrics = []workload.AutoscalingPolicyMetric{
		{Name: "decode_queue", TargetValue: resource.MustParse("10")},
		{Name: "prefill_queue", TargetValue: resource.MustParse("10")},
	}
	spec.HomogeneousTarget = &workload.HomogeneousTarget{
		Target: workload.Target{
			TargetRef: modelServingRef("model"),
			MetricSources: map[string]workload.MetricSource{
				"decode_queue":  {Pod: &workload.PodMetricSource{Name: "decode_queue", Port: port, LabelSelector: matchLabels(workload.RoleLabelKey, "decode")}},
				"prefill_queue": {Pod: &workload.PodMetricSource{Name: "prefill_queue", Port: port, LabelSelector: matchLabels(workload.RoleLabelKey, "prefill")}},
			},
		},
		MaxReplicas: 100,
	}
	policy := newPolicy(spec)

	got, err := NewAutoscaler(policy).Scale(context.Background(), podLister, policy, 2)

	// decode_queue: 1.5 x 2 groups = 3. prefill_queue: 3 x 2 groups = 6. The larger one wins.
	require.NoError(t, err)
	assert.Equal(t, int32(6), got)
}

func TestPrometheusScenarioDoesNotCountServingGroups(t *testing.T) {
	prometheus := promStub(t, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"30"]}]}}`, 0, 0)
	spec := scenarioPolicySpec()
	spec.HomogeneousTarget = &workload.HomogeneousTarget{
		Target: workload.Target{
			TargetRef: modelServingRef("model"),
			MetricSources: map[string]workload.MetricSource{
				replicaUnitsMetric: {Prometheus: &workload.PrometheusMetricSource{ServerURL: prometheus.URL, Query: "queue_depth"}},
			},
		},
		MaxReplicas: 100,
	}
	policy := newPolicy(spec)
	podLister, _ := newScenarioPods(t, 0, groups("model", "server", 2, 2))

	got, err := NewAutoscaler(policy).Scale(context.Background(), podLister, policy, 2)

	// 30 / 10 = 3 replicas, however many entry pods each replica has.
	require.NoError(t, err)
	assert.Equal(t, int32(3), got)
}

func TestHeterogeneousScenarios(t *testing.T) {
	testcases := []struct {
		name      string
		currentA  int32
		currentB  int32
		pods      []podSet
		load      float64
		selectorA *metav1.LabelSelector
		want      int32
	}{
		{name: "backends of different sizes: 1.5 x (2 + 3) groups", currentA: 2, currentB: 3, pods: append(groups("a", "server", 2, 2), groups("b", "server", 1, 1, 1)...), load: 15, want: 8},
		{name: "backends with one entry pod: 1.5 x (2 + 3) groups", currentA: 2, currentB: 3, pods: append(groups("a", "server", 1, 1), groups("b", "server", 1, 1, 1)...), load: 15, want: 8},
		{name: "scale down: 0.5 x (2 + 3) groups", currentA: 2, currentB: 3, pods: append(groups("a", "server", 2, 2), groups("b", "server", 1, 1, 1)...), load: 5, want: 3},
		{name: "within tolerance keeps the total", currentA: 2, currentB: 3, pods: append(groups("a", "server", 2, 2), groups("b", "server", 1, 1, 1)...), load: 10.5, want: 5},
		{name: "backend with no pods adds nothing: 1.5 x 4 groups", currentA: 4, currentB: 2, pods: groups("a", "server", 2, 2, 2, 2), load: 15, want: 6},
		{name: "backend partly created: 1.5 x (2 + 1) groups", currentA: 2, currentB: 3, pods: append(groups("a", "server", 2, 2), groups("b", "server", 1)...), load: 15, want: 5},
		{
			name: "unready pods in one backend: 90 / 7 pods / 10 x (2 + 3) groups", currentA: 2, currentB: 3, load: 15, want: 7,
			pods: append(groups("b", "server", 1, 1, 1),
				podSet{modelServing: "a", group: 0, role: "server", count: 2},
				podSet{modelServing: "a", group: 1, role: "server", count: 1},
				podSet{modelServing: "a", group: 1, role: "server", count: 1, state: podUnready}),
		},
		{
			name: "scale down leaves out a fully unready backend: 0.5 x 6 groups", currentA: 2, currentB: 6, load: 5, want: 3,
			pods: append(groups("b", "server", 1, 1, 1, 1, 1, 1),
				podSet{modelServing: "a", group: 0, role: "server", count: 4, state: podUnready},
				podSet{modelServing: "a", group: 1, role: "server", count: 4, state: podUnready}),
		},
		{name: "backend at zero replicas with pods alive: 1.5 x (1 + 2) groups", currentA: 0, currentB: 2, pods: append(groups("a", "server", 2), groups("b", "server", 1, 1)...), load: 15, want: 5},
		{
			name: "selector on one backend: 1.5 x (2 + 3) groups", currentA: 2, currentB: 3, load: 15, want: 8,
			pods:      append(append(groups("a", "prefill", 1, 1), groups("a", "decode", 2, 2)...), groups("b", "server", 1, 1, 1)...),
			selectorA: matchLabels(workload.RoleLabelKey, "prefill"),
		},
	}

	newOptimizerPolicy := func(port int32, selectorA *metav1.LabelSelector) *workload.AutoscalingPolicy {
		param := func(name string, selector *metav1.LabelSelector) workload.HeterogeneousTargetParam {
			return workload.HeterogeneousTargetParam{
				Target:      workload.Target{TargetRef: modelServingRef(name), MetricSources: podSources(port, selector)},
				Cost:        100,
				MaxReplicas: 100,
			}
		}
		spec := scenarioPolicySpec()
		spec.HeterogeneousTarget = &workload.HeterogeneousTarget{
			CostExpansionRatePercent: 100,
			Params:                   []workload.HeterogeneousTargetParam{param("a", selectorA), param("b", nil)},
		}
		return newPolicy(spec)
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			podLister, port := newScenarioPods(t, tc.load, tc.pods)
			policy := newOptimizerPolicy(port, tc.selectorA)

			replicas, err := NewOptimizer(policy).Optimize(context.Background(), podLister, policy, map[string]int32{"default/a": tc.currentA, "default/b": tc.currentB})

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas["default/a"]+replicas["default/b"])
		})
	}

	t.Run("no pods in any backend skips scaling", func(t *testing.T) {
		podLister, port := newScenarioPods(t, 15, nil)
		policy := newOptimizerPolicy(port, nil)

		replicas, err := NewOptimizer(policy).Optimize(context.Background(), podLister, policy, map[string]int32{"default/a": 2, "default/b": 3})

		require.NoError(t, err)
		assert.Nil(t, replicas)
	})
}

func TestDisaggregatedScenarios(t *testing.T) {
	testcases := []struct {
		name     string
		current  int32
		pods     []podSet
		load     float64
		selector *metav1.LabelSelector
		want     int32
	}{
		{name: "one replica in two groups: 1.5 x 2 pods / 2 groups", current: 1, pods: groups("model", "prefill", 1, 1), load: 15, want: 2},
		{name: "two replicas in two groups: 1.5 x 4 pods / 2 groups", current: 2, pods: groups("model", "prefill", 2, 2), load: 15, want: 3},
		{name: "scale down: 0.5 x 8 pods / 2 groups", current: 4, pods: groups("model", "prefill", 4, 4), load: 5, want: 2},
		{name: "one group: 1.5 x 2 pods / 1 group", current: 2, pods: groups("model", "prefill", 2), load: 15, want: 3},
		{name: "within tolerance keeps replicas", current: 2, pods: groups("model", "prefill", 2, 2), load: 10.5, want: 2},
		{name: "role replica partly created: 1.5 x 3 pods / 2 groups", current: 2, pods: groups("model", "prefill", 2, 1), load: 15, want: 3},
		{
			name: "unready pods on scale up: 80 / 4 pods / 10 x 4 pods / 2 groups", current: 2, load: 40, want: 4,
			pods: []podSet{
				{modelServing: "model", group: 0, role: "prefill", count: 2},
				{modelServing: "model", group: 1, role: "prefill", count: 2, state: podUnready},
			},
		},
		{name: "pods of other roles aren't counted: 1.5 x 2 pods / 2 groups", current: 1, pods: append(groups("model", "prefill", 1, 1), groups("model", "decode", 3, 3)...), load: 15, want: 2},
		{name: "selector on group-name: 1.5 x 1 pod / 1 group", current: 1, pods: groups("model", "prefill", 1, 1), load: 15, selector: matchLabels(workload.GroupNameLabelKey, "model-0"), want: 2},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			podLister, port := newScenarioPods(t, tc.load, tc.pods)
			spec := scenarioPolicySpec()
			spec.DisaggregatedTarget = &workload.DisaggregatedTarget{
				TargetRef: modelServingRef("model"),
				Roles: map[string]workload.RoleScalingParam{
					"prefill": {MinReplicas: 1, MaxReplicas: 100, MetricSources: podSources(port, tc.selector)},
				},
			}
			policy := newPolicy(spec)

			result, err := NewDisaggregatedAutoscaler(policy).Scale(context.Background(), podLister, policy, map[string]int32{"prefill": tc.current})

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, result.Roles, 1)
			assert.Equal(t, tc.want, result.Roles[0].FinalReplicas)
		})
	}
}

func TestCountServingGroups(t *testing.T) {
	withoutGroupLabel := func(pod *corev1.Pod, name string) *corev1.Pod {
		delete(pod.Labels, workload.GroupNameLabelKey)
		pod.Name = name
		return pod
	}
	ready := podSet{modelServing: "model", group: 0, role: "server", count: 1}
	unready := podSet{modelServing: "model", group: 1, role: "server", count: 1, state: podUnready}

	testcases := []struct {
		name string
		pods []*corev1.Pod
		want servingGroupCount
	}{
		{name: "no pods", want: servingGroupCount{}},
		{name: "two ready pods in one group", pods: []*corev1.Pod{newScenarioPod(ready, 0, 0), newScenarioPod(ready, 0, 1)}, want: servingGroupCount{ready: 1, all: 1}},
		{name: "a group with only unready pods counts in all", pods: []*corev1.Pod{newScenarioPod(ready, 0, 0), newScenarioPod(unready, 1, 0)}, want: servingGroupCount{ready: 1, all: 2}},
		{name: "one ready pod makes the group ready", pods: []*corev1.Pod{newScenarioPod(unready, 0, 0), newScenarioPod(podSet{modelServing: "model", group: 1, role: "server"}, 1, 1)}, want: servingGroupCount{ready: 1, all: 1}},
		{
			name: "a pod without the group label is its own group",
			pods: []*corev1.Pod{withoutGroupLabel(newScenarioPod(ready, 0, 0), "first"), withoutGroupLabel(newScenarioPod(ready, 0, 1), "second")},
			want: servingGroupCount{ready: 2, all: 2},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, countServingGroups(tc.pods))
		})
	}
}

func TestUpdateMetricsCountsServingGroupsPerMetric(t *testing.T) {
	port := metricsPort(t, "# TYPE decode_queue gauge\ndecode_queue 5\n# TYPE prefill_queue gauge\nprefill_queue 5\n")
	podLister := newPodLister(t, append(groups("model", "prefill", 1), groups("model", "decode", 2, 2, 2)...))
	target := &workload.Target{
		TargetRef: modelServingRef("model"),
		MetricSources: map[string]workload.MetricSource{
			"decode_queue":  {Pod: &workload.PodMetricSource{Name: "decode_queue", Port: port, LabelSelector: matchLabels(workload.RoleLabelKey, "decode")}},
			"prefill_queue": {Pod: &workload.PodMetricSource{Name: "prefill_queue", Port: port, LabelSelector: matchLabels(workload.RoleLabelKey, "prefill")}},
		},
	}
	collector := NewMetricCollector(target, newPolicy(workload.AutoscalingPolicySpec{}), map[string]float64{"decode_queue": 10, "prefill_queue": 10})

	_, _, _, servingGroups, err := collector.UpdateMetrics(context.Background(), podLister, target.MetricSources)

	require.NoError(t, err)
	assert.Equal(t, map[string]servingGroupCount{
		"decode_queue":  {ready: 3, all: 3},
		"prefill_queue": {ready: 1, all: 1},
	}, servingGroups)
}

func TestUpdateMetricsCountsServingGroupsForMetricsSharingAPodSource(t *testing.T) {
	port := metricsPort(t, "# TYPE first_queue gauge\nfirst_queue 5\n# TYPE second_queue gauge\nsecond_queue 5\n")
	podLister := newPodLister(t, groups("model", "server", 2, 2))
	target := &workload.Target{
		TargetRef: modelServingRef("model"),
		MetricSources: map[string]workload.MetricSource{
			"first_queue":  {Pod: &workload.PodMetricSource{Name: "first_queue", Port: port}},
			"second_queue": {Pod: &workload.PodMetricSource{Name: "second_queue", Port: port}},
		},
	}
	collector := NewMetricCollector(target, newPolicy(workload.AutoscalingPolicySpec{}), map[string]float64{"first_queue": 10, "second_queue": 10})

	_, _, _, servingGroups, err := collector.UpdateMetrics(context.Background(), podLister, target.MetricSources)

	require.NoError(t, err)
	assert.Equal(t, map[string]servingGroupCount{
		"first_queue":  {ready: 2, all: 2},
		"second_queue": {ready: 2, all: 2},
	}, servingGroups)
}
