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
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
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
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-a", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, ScalingConfiguration: &workload.HomogeneousTarget{Target: target, MinReplicas: 1, MaxReplicas: 100}}}

	lbs, _ := util.GetTargetLabels(&target)
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
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-up", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, ScalingConfiguration: &workload.HomogeneousTarget{Target: target, MinReplicas: 1, MaxReplicas: 10}}}

	lbs, _ := util.GetTargetLabels(&target)
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
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-b", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, OptimizerConfiguration: &workload.HeterogeneousTarget{Params: []workload.HeterogeneousTargetParam{paramA, paramB}, CostExpansionRatePercent: 100}}}

	lbsA, _ := util.GetTargetLabels(&paramA.Target)
	lbsB, _ := util.GetTargetLabels(&paramB.Target)
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
	binding := &workload.AutoscalingPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-b2", Namespace: ns}, Spec: workload.AutoscalingPolicyBindingSpec{PolicyRef: corev1.LocalObjectReference{Name: "ap"}, OptimizerConfiguration: &workload.HeterogeneousTarget{Params: []workload.HeterogeneousTargetParam{paramA, paramB}, CostExpansionRatePercent: 100}}}

	lbsA, _ := util.GetTargetLabels(&paramA.Target)
	lbsB, _ := util.GetTargetLabels(&paramB.Target)
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
