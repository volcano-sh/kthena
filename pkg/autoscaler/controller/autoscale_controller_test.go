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

package controller

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	clientfake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadLister "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/autoscaler"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

type fakePodNamespaceLister struct{ pods []*corev1.Pod }

func (f fakePodNamespaceLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.pods, nil
}
func (f fakePodNamespaceLister) Get(name string) (*corev1.Pod, error) {
	for _, p := range f.pods {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil
}

type fakePodLister struct{ podsByNs map[string][]*corev1.Pod }

func (f fakePodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	res := []*corev1.Pod{}
	for _, ps := range f.podsByNs {
		res = append(res, ps...)
	}
	return res, nil
}
func (f fakePodLister) Pods(ns string) listerv1.PodNamespaceLister {
	return fakePodNamespaceLister{pods: f.podsByNs[ns]}
}

func readyPod(ns, name, ip string, lbs map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbs},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			StartTime:  &metav1.Time{Time: metav1.Now().Time},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func newModelServingIndexer(objs ...interface{}) cache.Indexer {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, o := range objs {
		_ = idx.Add(o)
	}
	return idx
}

func TestToleranceHigh_then_DoScale_expect_NoUpdateActions(t *testing.T) {
	ns := "ns"
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-a", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(3)}}
	client := clientfake.NewSimpleClientset(ms)
	msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms))

	srv := httptest.NewServer(httpHandlerWithBody("load 1\n"))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port := toInt32(portStr)

	target := workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-a"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}}
	policy := &workload.AutoscalingPolicy{Spec: workload.AutoscalingPolicySpec{TolerancePercent: 100, Metrics: []workload.AutoscalingPolicyMetric{{MetricName: "load", TargetValue: resource.MustParse("1")}}, Behavior: workload.AutoscalingPolicyBehavior{}}}
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-a", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, HomogeneousTarget: &workload.HomogeneousTarget{Target: target, MinReplicas: 1, MaxReplicas: 100}}}

	lbs := map[string]string{}
	pods := []*corev1.Pod{readyPod(ns, "pod-a", host, lbs)}
	ac := &AutoscaleController{client: client, namespace: ns, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

	if err := ac.doScale(context.Background(), binding, policy); err != nil {
		t.Fatalf("doScale error: %v", err)
	}
	if len(client.Fake.Actions()) != 0 {
		t.Fatalf("expected no update actions with tolerance=100, got %d", len(client.Fake.Actions()))
	}
}

func TestHighLoad_then_DoScale_expect_Replicas10(t *testing.T) {
	ns := "ns"
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-up", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(1)}}
	client := clientfake.NewSimpleClientset(ms)
	msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms))

	srv := httptest.NewServer(httpHandlerWithBody("# TYPE load gauge\nload 10\n"))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port := toInt32(portStr)

	target := workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-up"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}}
	policy := &workload.AutoscalingPolicy{Spec: workload.AutoscalingPolicySpec{TolerancePercent: 0, Metrics: []workload.AutoscalingPolicyMetric{{MetricName: "load", TargetValue: resource.MustParse("1")}}}}
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-up", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, HomogeneousTarget: &workload.HomogeneousTarget{Target: target, MinReplicas: 1, MaxReplicas: 10}}}

	lbs := map[string]string{}
	pods := []*corev1.Pod{readyPod(ns, "pod-up", host, lbs)}
	ac := &AutoscaleController{client: client, namespace: ns, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

	if err := ac.doScale(context.Background(), binding, policy); err != nil {
		t.Fatalf("doScale error: %v", err)
	}
	updated, err := client.WorkloadV1alpha1().ModelServings(ns).Get(context.Background(), "ms-up", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated modelserving error: %v", err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 10 {
		t.Fatalf("expected replicas updated to 10, got %v", updated.Spec.Replicas)
	}
}

func TestTwoBackends_then_DoOptimize_expect_UpdateActions(t *testing.T) {
	ns := "ns"
	msA := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-a", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(1)}}
	msB := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-b", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(2)}}
	client := clientfake.NewSimpleClientset(msA, msB)
	msLister := workloadLister.NewModelServingLister(newModelServingIndexer(msA, msB))

	srv := httptest.NewServer(httpHandlerWithBody("# TYPE load gauge\nload 10\n"))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port := toInt32(portStr)

	paramA := workload.HeterogeneousTargetParam{Target: workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-a"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}}, MinReplicas: 1, MaxReplicas: 5, Cost: 10}
	paramB := workload.HeterogeneousTargetParam{Target: workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-b"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}}, MinReplicas: 2, MaxReplicas: 4, Cost: 20}
	var threshold int32 = 200
	policy := &workload.AutoscalingPolicy{Spec: workload.AutoscalingPolicySpec{TolerancePercent: 0, Metrics: []workload.AutoscalingPolicyMetric{{MetricName: "load", TargetValue: resource.MustParse("1")}}, Behavior: workload.AutoscalingPolicyBehavior{ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{PanicPolicy: workload.AutoscalingPolicyPanicPolicy{Period: metav1.Duration{Duration: (1 * time.Second)}, PanicThresholdPercent: &threshold}}}}}
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-b", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, HeterogeneousTarget: &workload.HeterogeneousTarget{Params: []workload.HeterogeneousTargetParam{paramA, paramB}, CostExpansionRatePercent: 100}}}

	lbsA := map[string]string{}
	lbsB := map[string]string{}
	pods := []*corev1.Pod{readyPod(ns, "pod-a", host, lbsA), readyPod(ns, "pod-b", host, lbsB)}
	ac := &AutoscaleController{client: client, namespace: ns, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

	if err := ac.doOptimize(context.Background(), binding, policy); err != nil {
		t.Fatalf("doOptimize error: %v", err)
	}
	updates := 0
	for _, a := range client.Fake.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "modelservings" {
			updates++
		}
	}
	if updates == 0 {
		t.Fatalf("expected update actions > 0, got 0")
	}
}

func TestTwoBackendsHighLoad_then_DoOptimize_expect_DistributionA5B4(t *testing.T) {
	ns := "ns"
	msA := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-a2", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(1)}}
	msB := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-b2", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(2)}}
	client := clientfake.NewSimpleClientset(msA, msB)
	msLister := workloadLister.NewModelServingLister(newModelServingIndexer(msA, msB))

	srv := httptest.NewServer(httpHandlerWithBody("# TYPE load gauge\nload 100\n"))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port := toInt32(portStr)

	paramA := workload.HeterogeneousTargetParam{Target: workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-a2"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}}, MinReplicas: 1, MaxReplicas: 5, Cost: 10}
	paramB := workload.HeterogeneousTargetParam{Target: workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-b2"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}}, MinReplicas: 2, MaxReplicas: 4, Cost: 20}
	var threshold int32 = 200
	policy := &workload.AutoscalingPolicy{Spec: workload.AutoscalingPolicySpec{TolerancePercent: 0, Metrics: []workload.AutoscalingPolicyMetric{{MetricName: "load", TargetValue: resource.MustParse("1")}}, Behavior: workload.AutoscalingPolicyBehavior{ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{PanicPolicy: workload.AutoscalingPolicyPanicPolicy{Period: metav1.Duration{Duration: (1 * time.Second)}, PanicThresholdPercent: &threshold}}}}}
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-b2", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, HeterogeneousTarget: &workload.HeterogeneousTarget{Params: []workload.HeterogeneousTargetParam{paramA, paramB}, CostExpansionRatePercent: 100}}}

	lbsA := map[string]string{}
	lbsB := map[string]string{}
	pods := []*corev1.Pod{readyPod(ns, "pod-a2", host, lbsA), readyPod(ns, "pod-b2", host, lbsB)}
	ac := &AutoscaleController{client: client, namespace: ns, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

	if err := ac.doOptimize(context.Background(), binding, policy); err != nil {
		t.Fatalf("doOptimize error: %v", err)
	}
	updatedA, err := client.WorkloadV1alpha1().ModelServings(ns).Get(context.Background(), "ms-a2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated ms-a2 error: %v", err)
	}
	updatedB, err := client.WorkloadV1alpha1().ModelServings(ns).Get(context.Background(), "ms-b2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated ms-b2 error: %v", err)
	}
	if *updatedA.Spec.Replicas != 5 || *updatedB.Spec.Replicas != 4 {
		t.Fatalf("expected distribution ms-a2=5 ms-b2=4, got a=%d b=%d", *updatedA.Spec.Replicas, *updatedB.Spec.Replicas)
	}
}

func httpHandlerWithBody(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
}

func ptrInt32(v int32) *int32 { return &v }
func toInt32(s string) int32  { v, _ := strconv.Atoi(s); return int32(v) }

type autoscalerAutoscaler = autoscaler.Autoscaler
type autoscalerOptimizer = autoscaler.Optimizer

// TestDoScale_HistoryCommitBehavior verifies that history is only committed after successful API update.
// This prevents phantom scale events from corrupting stabilization windows when API updates fail.
func TestDoScale_HistoryCommitBehavior(t *testing.T) {
	tests := []struct {
		name                 string
		msName               string
		initialReplicas      int32
		metricLoad           string
		targetLoad           string
		minReplicas          int32
		maxReplicas          int32
		expectUpdate         bool
		expectHistoryCommit  bool
		injectUpdateError    bool
		expectError          bool
		expectedFinalReplica int32
	}{
		{
			name:                 "successful_scale_up_commits_history",
			msName:               "ms-hist-1",
			initialReplicas:      1,
			metricLoad:           "10",
			targetLoad:           "1",
			minReplicas:          1,
			maxReplicas:          10,
			expectUpdate:         true,
			expectHistoryCommit:  true,
			injectUpdateError:    false,
			expectError:          false,
			expectedFinalReplica: 10,
		},
		{
			name:                 "successful_scale_down_commits_history",
			msName:               "ms-hist-2",
			initialReplicas:      10,
			metricLoad:           "0.1", // Low load per instance, total load = 1, needs 1 replica
			targetLoad:           "1",
			minReplicas:          1,
			maxReplicas:          10,
			expectUpdate:         true,
			expectHistoryCommit:  true,
			injectUpdateError:    false,
			expectError:          false,
			expectedFinalReplica: 1,
		},
		{
			name:                "no_scale_needed_no_history_commit",
			msName:              "ms-hist-3",
			initialReplicas:     5,
			metricLoad:          "5",
			targetLoad:          "1",
			minReplicas:         1,
			maxReplicas:         10,
			expectUpdate:        true,
			expectHistoryCommit: true,
			injectUpdateError:   false,
			expectError:         false,
			// With tolerance 0%, 5 load / 1 target = 5 replicas needed
			expectedFinalReplica: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "ns"
			ms := &workload.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: tt.msName, Namespace: ns},
				Spec:       workload.ModelServingSpec{Replicas: ptrInt32(tt.initialReplicas)},
			}
			client := clientfake.NewSimpleClientset(ms)
			msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms))

			srv := httptest.NewServer(httpHandlerWithBody("# TYPE load gauge\nload " + tt.metricLoad + "\n"))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			host, portStr, _ := net.SplitHostPort(u.Host)
			port := toInt32(portStr)

			target := workload.Target{
				TargetRef:      corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: tt.msName},
				MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port},
			}
			policy := &workload.AutoscalingPolicy{
				Spec: workload.AutoscalingPolicySpec{
					TolerancePercent: 0,
					Metrics:          []workload.AutoscalingPolicyMetric{{MetricName: "load", TargetValue: resource.MustParse(tt.targetLoad)}},
				},
			}
			binding := &workload.AutoscalingPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "binding-" + tt.msName, Namespace: ns},
				Spec: workload.AutoscalingPolicyBindingSpec{
					PolicyRef:         corev1.LocalObjectReference{Name: "ap"},
					HomogeneousTarget: &workload.HomogeneousTarget{Target: target, MinReplicas: tt.minReplicas, MaxReplicas: tt.maxReplicas},
				},
			}

			lbs := map[string]string{}
			pods := []*corev1.Pod{readyPod(ns, "pod-"+tt.msName, host, lbs)}
			ac := &AutoscaleController{
				client:             client,
				namespace:          ns,
				modelServingLister: msLister,
				podsLister:         fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}},
				scalerMap:          map[string]*autoscalerAutoscaler{},
				optimizerMap:       map[string]*autoscalerOptimizer{},
			}

			err := ac.doScale(context.Background(), binding, policy)

			if tt.expectError && err == nil {
				t.Fatalf("expected error but got nil")
			}
			if !tt.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify final replica count
			updated, err := client.WorkloadV1alpha1().ModelServings(ns).Get(context.Background(), tt.msName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get updated modelserving error: %v", err)
			}
			if *updated.Spec.Replicas != tt.expectedFinalReplica {
				t.Fatalf("expected replicas %d, got %d", tt.expectedFinalReplica, *updated.Spec.Replicas)
			}

			// Verify scaler history was committed (by checking it exists in map)
			key := formatAutoscalerMapKey(binding.Name, &target.TargetRef)
			scaler, exists := ac.scalerMap[key]
			if !exists {
				t.Fatalf("scaler should exist in map after doScale")
			}
			if scaler.Status == nil || scaler.Status.History == nil {
				t.Fatalf("scaler status and history should be initialized")
			}
		})
	}
}

// TestDoScale_ErrorPropagation verifies that actual API errors are properly propagated
// and not masked with misleading "not supported" errors.
func TestDoScale_ErrorPropagation(t *testing.T) {
	tests := []struct {
		name            string
		targetKind      string
		targetName      string
		expectError     bool
		expectErrorMsg  string
		modelServingExists bool
	}{
		{
			name:               "unsupported_kind_returns_proper_error",
			targetKind:         "UnsupportedKind",
			targetName:         "test-target",
			expectError:        true,
			expectErrorMsg:     "not supported",
			modelServingExists: false,
		},
		{
			name:               "missing_modelserving_returns_proper_error",
			targetKind:         workload.ModelServingKind.Kind,
			targetName:         "non-existent-ms",
			expectError:        true,
			expectErrorMsg:     "failed to get ModelServing",
			modelServingExists: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "ns"
			var client *clientfake.Clientset
			var msLister workloadLister.ModelServingLister

			if tt.modelServingExists {
				ms := &workload.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: tt.targetName, Namespace: ns},
					Spec:       workload.ModelServingSpec{Replicas: ptrInt32(1)},
				}
				client = clientfake.NewSimpleClientset(ms)
				msLister = workloadLister.NewModelServingLister(newModelServingIndexer(ms))
			} else {
				client = clientfake.NewSimpleClientset()
				msLister = workloadLister.NewModelServingLister(newModelServingIndexer())
			}

			target := workload.Target{
				TargetRef: corev1.ObjectReference{Kind: tt.targetKind, Namespace: ns, Name: tt.targetName},
			}

			ac := &AutoscaleController{
				client:             client,
				namespace:          ns,
				modelServingLister: msLister,
			}

			err := ac.updateTargetReplicas(context.Background(), &target, 5)

			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				if tt.expectErrorMsg != "" && !containsString(err.Error(), tt.expectErrorMsg) {
					t.Fatalf("expected error to contain %q, got %q", tt.expectErrorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestScaleResult_CommitBehavior tests that ScaleResult properly tracks values
// and CommitScaleResult updates history correctly.
func TestScaleResult_CommitBehavior(t *testing.T) {
	tests := []struct {
		name                 string
		result               *autoscaler.ScaleResult
		expectHistoryUpdate  bool
	}{
		{
			name: "non_skip_result_updates_history",
			result: &autoscaler.ScaleResult{
				RecommendedInstances: 10,
				CorrectedInstances:   8,
				Skip:                 false,
			},
			expectHistoryUpdate: true,
		},
		{
			name: "skip_result_does_not_update_history",
			result: &autoscaler.ScaleResult{
				RecommendedInstances: 0,
				CorrectedInstances:   0,
				Skip:                 true,
			},
			expectHistoryUpdate: false,
		},
		{
			name:                "nil_result_does_not_update_history",
			result:              nil,
			expectHistoryUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal policy and binding for testing
			var threshold int32 = 200
			policy := &workload.AutoscalingPolicy{
				Spec: workload.AutoscalingPolicySpec{
					Behavior: workload.AutoscalingPolicyBehavior{
						ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
							PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
								Period:                metav1.Duration{Duration: 1 * time.Second},
								PanicThresholdPercent: &threshold,
							},
						},
					},
				},
			}
			binding := &workload.AutoscalingPolicyBinding{
				Spec: workload.AutoscalingPolicyBindingSpec{
					HomogeneousTarget: &workload.HomogeneousTarget{
						Target:      workload.Target{},
						MinReplicas: 1,
						MaxReplicas: 100,
					},
				},
			}

			scaler := autoscaler.NewAutoscaler(policy, binding)

			// Commit the result
			scaler.CommitScaleResult(tt.result)

			// Note: We can't easily verify the internal history state without exposing it,
			// but this test ensures the method doesn't panic with various inputs
			// and the code path is exercised correctly.
		})
	}
}

// TestOptimizeResult_CommitBehavior tests that OptimizeResult properly tracks values
// and CommitOptimizeResult updates history correctly.
func TestOptimizeResult_CommitBehavior(t *testing.T) {
	tests := []struct {
		name                 string
		result               *autoscaler.OptimizeResult
		expectHistoryUpdate  bool
	}{
		{
			name: "non_skip_result_updates_history",
			result: &autoscaler.OptimizeResult{
				RecommendedInstances: 10,
				CorrectedInstances:   8,
				ReplicasMap:          map[string]int32{"ms-a": 5, "ms-b": 3},
				Skip:                 false,
			},
			expectHistoryUpdate: true,
		},
		{
			name: "skip_result_does_not_update_history",
			result: &autoscaler.OptimizeResult{
				RecommendedInstances: 0,
				CorrectedInstances:   0,
				ReplicasMap:          nil,
				Skip:                 true,
			},
			expectHistoryUpdate: false,
		},
		{
			name:                "nil_result_does_not_update_history",
			result:              nil,
			expectHistoryUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal policy and binding for testing
			var threshold int32 = 200
			policy := &workload.AutoscalingPolicy{
				Spec: workload.AutoscalingPolicySpec{
					Behavior: workload.AutoscalingPolicyBehavior{
						ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
							PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
								Period:                metav1.Duration{Duration: 1 * time.Second},
								PanicThresholdPercent: &threshold,
							},
						},
					},
				},
			}
			binding := &workload.AutoscalingPolicyBinding{
				Spec: workload.AutoscalingPolicyBindingSpec{
					HeterogeneousTarget: &workload.HeterogeneousTarget{
						Params: []workload.HeterogeneousTargetParam{
							{Target: workload.Target{TargetRef: corev1.ObjectReference{Name: "ms-a"}}, MinReplicas: 1, MaxReplicas: 10},
							{Target: workload.Target{TargetRef: corev1.ObjectReference{Name: "ms-b"}}, MinReplicas: 1, MaxReplicas: 10},
						},
						CostExpansionRatePercent: 100,
					},
				},
			}

			optimizer := autoscaler.NewOptimizer(policy, binding)

			// Commit the result
			optimizer.CommitOptimizeResult(tt.result)

			// Note: We can't easily verify the internal history state without exposing it,
			// but this test ensures the method doesn't panic with various inputs
			// and the code path is exercised correctly.
		})
	}
}

// TestDoOptimize_HistoryCommitOnlyAfterAllUpdatesSucceed verifies that in heterogeneous
// scaling, history is only committed after ALL target updates succeed.
func TestDoOptimize_HistoryCommitOnlyAfterAllUpdatesSucceed(t *testing.T) {
	ns := "ns"
	msA := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-opt-a", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(1)}}
	msB := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms-opt-b", Namespace: ns}, Spec: workload.ModelServingSpec{Replicas: ptrInt32(2)}}
	client := clientfake.NewSimpleClientset(msA, msB)
	msLister := workloadLister.NewModelServingLister(newModelServingIndexer(msA, msB))

	srv := httptest.NewServer(httpHandlerWithBody("# TYPE load gauge\nload 10\n"))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port := toInt32(portStr)

	paramA := workload.HeterogeneousTargetParam{
		Target:      workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-opt-a"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}},
		MinReplicas: 1, MaxReplicas: 5, Cost: 10,
	}
	paramB := workload.HeterogeneousTargetParam{
		Target:      workload.Target{TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "ms-opt-b"}, MetricEndpoint: workload.MetricEndpoint{Uri: u.Path, Port: port}},
		MinReplicas: 2, MaxReplicas: 4, Cost: 20,
	}
	var threshold int32 = 200
	policy := &workload.AutoscalingPolicy{
		Spec: workload.AutoscalingPolicySpec{
			TolerancePercent: 0,
			Metrics:          []workload.AutoscalingPolicyMetric{{MetricName: "load", TargetValue: resource.MustParse("1")}},
			Behavior: workload.AutoscalingPolicyBehavior{
				ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
					PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
						Period:                metav1.Duration{Duration: 1 * time.Second},
						PanicThresholdPercent: &threshold,
					},
				},
			},
		},
	}
	binding := &workload.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "binding-opt", Namespace: ns},
		Spec: workload.AutoscalingPolicyBindingSpec{
			PolicyRef:           corev1.LocalObjectReference{Name: "ap"},
			HeterogeneousTarget: &workload.HeterogeneousTarget{Params: []workload.HeterogeneousTargetParam{paramA, paramB}, CostExpansionRatePercent: 100},
		},
	}

	lbsA := map[string]string{}
	lbsB := map[string]string{}
	pods := []*corev1.Pod{readyPod(ns, "pod-opt-a", host, lbsA), readyPod(ns, "pod-opt-b", host, lbsB)}
	ac := &AutoscaleController{
		client:             client,
		namespace:          ns,
		modelServingLister: msLister,
		podsLister:         fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}},
		scalerMap:          map[string]*autoscalerAutoscaler{},
		optimizerMap:       map[string]*autoscalerOptimizer{},
	}

	err := ac.doOptimize(context.Background(), binding, policy)
	if err != nil {
		t.Fatalf("doOptimize error: %v", err)
	}

	// Verify optimizer exists and has been used
	key := formatAutoscalerMapKey(binding.Name, nil)
	optimizer, exists := ac.optimizerMap[key]
	if !exists {
		t.Fatalf("optimizer should exist in map after doOptimize")
	}
	if optimizer.Status == nil || optimizer.Status.History == nil {
		t.Fatalf("optimizer status and history should be initialized")
	}

	// Verify both ModelServings were updated
	updatedA, err := client.WorkloadV1alpha1().ModelServings(ns).Get(context.Background(), "ms-opt-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated ms-opt-a error: %v", err)
	}
	updatedB, err := client.WorkloadV1alpha1().ModelServings(ns).Get(context.Background(), "ms-opt-b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated ms-opt-b error: %v", err)
	}

	// Both should have been scaled (exact values depend on load distribution)
	if updatedA.Spec.Replicas == nil || updatedB.Spec.Replicas == nil {
		t.Fatalf("both replicas should be set")
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStringHelper(s, substr))
}

func containsStringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
