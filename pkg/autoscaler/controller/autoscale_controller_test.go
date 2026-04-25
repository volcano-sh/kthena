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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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
	k8stesting "k8s.io/client-go/testing"
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
	ac := &AutoscaleController{client: client, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

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
	ac := &AutoscaleController{client: client, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

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

func TestTwoBackends_then_DoOptimize_expect_PatchActions(t *testing.T) {
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
	ac := &AutoscaleController{client: client, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

	if err := ac.doOptimize(context.Background(), binding, policy); err != nil {
		t.Fatalf("doOptimize error: %v", err)
	}
	updates := 0
	for _, a := range client.Fake.Actions() {
		if (a.GetVerb() == "update" || a.GetVerb() == "patch") && a.GetResource().Resource == "modelservings" {
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
	ac := &AutoscaleController{client: client, modelServingLister: msLister, podsLister: fakePodLister{podsByNs: map[string][]*corev1.Pod{ns: pods}}, scalerMap: map[string]*autoscalerAutoscaler{}, optimizerMap: map[string]*autoscalerOptimizer{}}

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

func TestFormatAutoscalerMapKey_IncludesNamespaceAndTarget(t *testing.T) {
	targetRef := &corev1.ObjectReference{Name: "same-target", Kind: workload.ModelServingKind.Kind}

	// Different binding namespaces, same binding name and target → distinct keys.
	keyA := formatAutoscalerMapKey("team-ml", "shared-binding", targetRef)
	keyB := formatAutoscalerMapKey("team-ai", "shared-binding", targetRef)
	if keyA == keyB {
		t.Fatalf("expected different keys for different binding namespaces, got identical key %q", keyA)
	}

	// Same namespace and binding, different target names → distinct keys.
	ref1 := &corev1.ObjectReference{Name: "target-1", Kind: workload.ModelServingKind.Kind}
	ref2 := &corev1.ObjectReference{Name: "target-2", Kind: workload.ModelServingKind.Kind}
	key1 := formatAutoscalerMapKey("ns", "binding", ref1)
	key2 := formatAutoscalerMapKey("ns", "binding", ref2)
	if key1 == key2 {
		t.Fatalf("expected different keys for different target names, got identical key %q", key1)
	}

	// Same namespace and binding, different target kinds → distinct keys.
	refKindA := &corev1.ObjectReference{Name: "target", Kind: "KindA"}
	refKindB := &corev1.ObjectReference{Name: "target", Kind: "KindB"}
	keyKindA := formatAutoscalerMapKey("ns", "binding", refKindA)
	keyKindB := formatAutoscalerMapKey("ns", "binding", refKindB)
	if keyKindA == keyKindB {
		t.Fatalf("expected different keys for different target kinds, got identical key %q", keyKindA)
	}
}

func TestFormatAutoscalerMapKey_TargetNamespaceDifferentiation(t *testing.T) {
	// Same binding, same target name/kind, different explicit target namespaces → distinct keys.
	refNsA := &corev1.ObjectReference{Name: "target", Kind: workload.ModelServingKind.Kind, Namespace: "ns-a"}
	refNsB := &corev1.ObjectReference{Name: "target", Kind: workload.ModelServingKind.Kind, Namespace: "ns-b"}
	keyA := formatAutoscalerMapKey("default", "binding", refNsA)
	keyB := formatAutoscalerMapKey("default", "binding", refNsB)
	if keyA == keyB {
		t.Fatalf("expected different keys for different target namespaces, got identical key %q", keyA)
	}

	// Explicit target namespace matching binding namespace vs empty (defaults to binding namespace) → same key.
	refExplicit := &corev1.ObjectReference{Name: "target", Kind: workload.ModelServingKind.Kind, Namespace: "ns"}
	refImplicit := &corev1.ObjectReference{Name: "target", Kind: workload.ModelServingKind.Kind}
	keyExplicit := formatAutoscalerMapKey("ns", "binding", refExplicit)
	keyImplicit := formatAutoscalerMapKey("ns", "binding", refImplicit)
	if keyExplicit != keyImplicit {
		t.Fatalf("expected same key when explicit target namespace matches binding namespace, got %q vs %q", keyExplicit, keyImplicit)
	}
}

func TestFormatAutoscalerMapKey_OptimizerIncludesNamespace(t *testing.T) {
	// Different namespaces, same binding name, nil targetRef (optimizer) → distinct keys.
	keyA := formatAutoscalerMapKey("team-a", "shared-binding", nil)
	keyB := formatAutoscalerMapKey("team-b", "shared-binding", nil)
	if keyA == keyB {
		t.Fatalf("expected different optimizer keys for different namespaces, got identical key %q", keyA)
	}

	// Same namespace, different binding names → distinct keys.
	key1 := formatAutoscalerMapKey("ns", "binding-1", nil)
	key2 := formatAutoscalerMapKey("ns", "binding-2", nil)
	if key1 == key2 {
		t.Fatalf("expected different optimizer keys for different bindings, got identical key %q", key1)
	}
}

// TestPatchReplicasDoesNotTouchResourceLimits verifies that updateTargetReplicas
// using Patch only sends the replicas field and never includes resources.limits
// in the patch body. This prevents the Quantity normalization issue ("0.2" → "200m")
// that caused unintended rolling updates.
func TestPatchReplicasDoesNotTouchResourceLimits(t *testing.T) {
	ns := "default"

	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: ns},
		Spec: workload.ModelServingSpec{
			Replicas: ptrInt32(1),
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptrInt32(1),
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "model",
										Image: "model:latest",
										Resources: corev1.ResourceRequirements{
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("0.2"),
												corev1.ResourceMemory: resource.MustParse("1Gi"),
											},
										},
									},
								},
							},
						},
					},
					{
						Name:     "decode",
						Replicas: ptrInt32(2),
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "model",
										Image: "model:latest",
										Resources: corev1.ResourceRequirements{
											Limits: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("0.5"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name            string
		target          workload.Target
		newReplicas     int32
		expectPatchVerb bool
	}{
		{
			name: "patch spec.replicas (MergePatch)",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms"},
			},
			newReplicas:     3,
			expectPatchVerb: true,
		},
		{
			name: "patch role prefill replicas (JSONPatch)",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms"},
				SubTarget: &workload.SubTarget{Kind: util.ModelServingRoleKind, Name: "prefill"},
			},
			newReplicas:     5,
			expectPatchVerb: true,
		},
		{
			name: "patch role decode replicas (JSONPatch)",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms"},
				SubTarget: &workload.SubTarget{Kind: util.ModelServingRoleKind, Name: "decode"},
			},
			newReplicas:     4,
			expectPatchVerb: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := clientfake.NewSimpleClientset(ms.DeepCopy())
			msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms.DeepCopy()))

			ac := &AutoscaleController{
				client:             fakeClient,
				modelServingLister: msLister,
				scalerMap:          map[string]*autoscalerAutoscaler{},
				optimizerMap:       map[string]*autoscalerOptimizer{},
			}

			err := ac.updateTargetReplicas(context.Background(), &tt.target, ns, tt.newReplicas)
			if err != nil {
				t.Fatalf("updateTargetReplicas error: %v", err)
			}

			// Find the patch action
			var patchAction k8stesting.PatchAction
			for _, action := range fakeClient.Actions() {
				if action.GetVerb() == "patch" {
					pa, ok := action.(k8stesting.PatchAction)
					if ok {
						patchAction = pa
						break
					}
				}
			}

			if tt.expectPatchVerb && patchAction == nil {
				t.Fatal("expected a patch action but found none")
			}

			patchBody := string(patchAction.GetPatch())
			t.Logf("Patch body: %s", patchBody)

			// The patch body must NOT contain any resource-related fields
			forbiddenFields := []string{"cpu", "memory", "resources", "limits", "requests", "image", "containers", "entryTemplate"}
			for _, field := range forbiddenFields {
				if strings.Contains(patchBody, field) {
					t.Errorf("patch body contains forbidden field %q — this would cause Quantity normalization issues.\nPatch: %s", field, patchBody)
				}
			}

			// The patch body MUST contain the replicas value
			if !strings.Contains(patchBody, fmt.Sprintf("%d", tt.newReplicas)) {
				t.Errorf("patch body does not contain the expected replicas value %d.\nPatch: %s", tt.newReplicas, patchBody)
			}

			// For role-level patches, verify it's a valid JSON Patch targeting only replicas
			if tt.target.SubTarget != nil {
				var ops []map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &ops); err != nil {
					t.Fatalf("failed to parse JSON Patch: %v", err)
				}
				fmt.Printf("ops: %v\n", ops)
				if len(ops) != 2 {
					t.Fatalf("expected exactly 1 JSON Patch operation, got %d", len(ops))
				}
				op := ops[1]
				if op["op"] != "add" {
					t.Errorf("expected op=add, got %v", op["op"])
				}
				path, _ := op["path"].(string)
				if !strings.HasSuffix(path, "/replicas") {
					t.Errorf("expected path ending with /replicas, got %q", path)
				}
				if !strings.HasPrefix(path, "/spec/template/roles/") {
					t.Errorf("expected path starting with /spec/template/roles/, got %q", path)
				}
			}
		})
	}
}

// TestPatchRoleReplicasPreservesOtherRoles verifies that patching one role's replicas
// does not affect other roles in the ModelServing spec.
func TestPatchRoleReplicasPreservesOtherRoles(t *testing.T) {
	ns := "default"

	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: ns},
		Spec: workload.ModelServingSpec{
			Replicas: ptrInt32(1),
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptrInt32(2),
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "model", Image: "model:latest"}},
							},
						},
					},
					{
						Name:     "decode",
						Replicas: ptrInt32(3),
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "model", Image: "model:latest"}},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := clientfake.NewSimpleClientset(ms.DeepCopy())
	msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms.DeepCopy()))

	ac := &AutoscaleController{
		client:             fakeClient,
		modelServingLister: msLister,
		scalerMap:          map[string]*autoscalerAutoscaler{},
		optimizerMap:       map[string]*autoscalerOptimizer{},
	}

	// Patch only the "prefill" role replicas
	target := workload.Target{
		TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms"},
		SubTarget: &workload.SubTarget{Kind: util.ModelServingRoleKind, Name: "prefill"},
	}

	err := ac.updateTargetReplicas(context.Background(), &target, ns, 10)
	if err != nil {
		t.Fatalf("updateTargetReplicas error: %v", err)
	}

	// Verify exactly one patch was issued, and that it only targets role index 0 (prefill).
	var patchBody string
	patchCount := 0
	updateCount := 0
	for _, action := range fakeClient.Actions() {
		if pa, ok := action.(k8stesting.PatchAction); ok {
			patchCount++
			patchBody = string(pa.GetPatch())
		}
		if _, ok := action.(k8stesting.UpdateAction); ok {
			updateCount++
		}
	}
	if patchCount != 1 {
		t.Fatalf("expected exactly one patch action, got %d actions: %#v", patchCount, fakeClient.Actions())
	}
	if updateCount != 0 {
		t.Fatalf("expected no update actions, got %d actions: %#v", updateCount, fakeClient.Actions())
	}
	// Must target roles/0 (prefill), not roles/1 (decode)
	if !strings.Contains(patchBody, "/spec/template/roles/0/replicas") {
		t.Errorf("expected patch to target roles/0, got: %s", patchBody)
	}
	if strings.Contains(patchBody, "/spec/template/roles/1") {
		t.Errorf("patch should not touch roles/1 (decode), got: %s", patchBody)
	}
}

// TestPatchSkipsWhenReplicasUnchanged verifies that no patch is issued if the
// target replicas already match the desired value.
func TestPatchSkipsWhenReplicasUnchanged(t *testing.T) {
	ns := "default"

	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: ns},
		Spec: workload.ModelServingSpec{
			Replicas: ptrInt32(3),
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptrInt32(5),
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "model", Image: "model:latest"}},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name     string
		target   workload.Target
		replicas int32
	}{
		{
			name: "spec.replicas unchanged",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms"},
			},
			replicas: 3,
		},
		{
			name: "role.replicas unchanged",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms"},
				SubTarget: &workload.SubTarget{Kind: util.ModelServingRoleKind, Name: "prefill"},
			},
			replicas: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := clientfake.NewSimpleClientset(ms.DeepCopy())
			msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms.DeepCopy()))

			ac := &AutoscaleController{
				client:             fakeClient,
				modelServingLister: msLister,
				scalerMap:          map[string]*autoscalerAutoscaler{},
				optimizerMap:       map[string]*autoscalerOptimizer{},
			}

			err := ac.updateTargetReplicas(context.Background(), &tt.target, ns, tt.replicas)
			if err != nil {
				t.Fatalf("updateTargetReplicas error: %v", err)
			}

			for _, action := range fakeClient.Actions() {
				if action.GetVerb() == "patch" {
					t.Fatalf("expected no patch when replicas unchanged, but got patch action")
				}
			}
		})
	}
}

// TestPatchDoesNotMutateResourcesInFakeClient verifies the full round-trip:
// create a ModelServing with cpu "0.2" → patch replicas via updateTargetReplicas
// → Get the object back from the fake client → resources.limits must be unchanged.
//
// This proves that the Patch approach does not cause Quantity normalization ("0.2" → "200m")
// unlike the old Update() approach which serialized the entire DeepCopy'd object.
func TestPatchDoesNotMutateResourcesInFakeClient(t *testing.T) {
	ns := "default"

	// Use JSON unmarshal to simulate how the API server stores "0.2" —
	// this preserves the original string representation in the Quantity.
	msJSON := `{
		"apiVersion": "workload.volcano.sh/v1alpha1",
		"kind": "ModelServing",
		"metadata": {"name": "test-ms", "namespace": "default"},
		"spec": {
			"replicas": 1,
			"template": {
				"roles": [{
					"name": "prefill",
					"replicas": 2,
					"entryTemplate": {
						"spec": {
							"containers": [{
								"name": "model",
								"image": "model:v1",
								"resources": {
									"limits": {"cpu": "0.2", "memory": "1Gi"},
									"requests": {"cpu": "0.1", "memory": "512Mi"}
								}
							}]
						}
					}
				}, {
					"name": "decode",
					"replicas": 3,
					"entryTemplate": {
						"spec": {
							"containers": [{
								"name": "model",
								"image": "model:v1",
								"resources": {
									"limits": {"cpu": "0.5", "memory": "2Gi"}
								}
							}]
						}
					}
				}]
			}
		}
	}`

	var ms workload.ModelServing
	if err := json.Unmarshal([]byte(msJSON), &ms); err != nil {
		t.Fatalf("failed to unmarshal test ModelServing: %v", err)
	}

	// Record original resource values (before any patch)
	origPrefillCPU := ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	origPrefillMem := ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	origDecodeCPU := ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	origDecodeMem := ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	origImage := ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image

	tests := []struct {
		name        string
		target      workload.Target
		newReplicas int32
	}{
		{
			name: "patch spec.replicas does not mutate resources",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{
					Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms",
				},
			},
			newReplicas: 5,
		},
		{
			name: "patch prefill role replicas does not mutate resources",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{
					Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms",
				},
				SubTarget: &workload.SubTarget{Kind: util.ModelServingRoleKind, Name: "prefill"},
			},
			newReplicas: 10,
		},
		{
			name: "patch decode role replicas does not mutate resources",
			target: workload.Target{
				TargetRef: corev1.ObjectReference{
					Kind: workload.ModelServingKind.Kind, Namespace: ns, Name: "test-ms",
				},
				SubTarget: &workload.SubTarget{Kind: util.ModelServingRoleKind, Name: "decode"},
			},
			newReplicas: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fresh fake client for each subtest with the original object
			fakeClient := clientfake.NewSimpleClientset(ms.DeepCopy())
			msLister := workloadLister.NewModelServingLister(newModelServingIndexer(ms.DeepCopy()))

			ac := &AutoscaleController{
				client:             fakeClient,
				modelServingLister: msLister,
				scalerMap:          map[string]*autoscalerAutoscaler{},
				optimizerMap:       map[string]*autoscalerOptimizer{},
			}

			// Perform the patch
			err := ac.updateTargetReplicas(context.Background(), &tt.target, ns, tt.newReplicas)
			if err != nil {
				t.Fatalf("updateTargetReplicas error: %v", err)
			}

			// Get the object back from the fake client store
			updated, err := fakeClient.WorkloadV1alpha1().ModelServings(ns).Get(
				context.Background(), "test-ms", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get updated ModelServing: %v", err)
			}

			// Verify replicas was actually changed
			if tt.target.SubTarget == nil {
				if updated.Spec.Replicas == nil || *updated.Spec.Replicas != tt.newReplicas {
					t.Errorf("expected spec.replicas=%d, got %v", tt.newReplicas, updated.Spec.Replicas)
				}
			} else {
				for _, role := range updated.Spec.Template.Roles {
					if role.Name == tt.target.SubTarget.Name {
						if role.Replicas == nil || *role.Replicas != tt.newReplicas {
							t.Errorf("expected role %s replicas=%d, got %v",
								tt.target.SubTarget.Name, tt.newReplicas, role.Replicas)
						}
					}
				}
			}

			// Verify resources.limits are UNCHANGED for all roles
			gotPrefillCPU := updated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
			gotPrefillMem := updated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
			gotDecodeCPU := updated.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
			gotDecodeMem := updated.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]

			if origPrefillCPU.Cmp(gotPrefillCPU) != 0 {
				t.Errorf("prefill CPU limit changed: %s → %s", origPrefillCPU.String(), gotPrefillCPU.String())
			}
			if origPrefillMem.Cmp(gotPrefillMem) != 0 {
				t.Errorf("prefill memory limit changed: %s → %s", origPrefillMem.String(), gotPrefillMem.String())
			}
			if origDecodeCPU.Cmp(gotDecodeCPU) != 0 {
				t.Errorf("decode CPU limit changed: %s → %s", origDecodeCPU.String(), gotDecodeCPU.String())
			}
			if origDecodeMem.Cmp(gotDecodeMem) != 0 {
				t.Errorf("decode memory limit changed: %s → %s", origDecodeMem.String(), gotDecodeMem.String())
			}

			// Verify image is unchanged
			gotImage := updated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image
			if gotImage != origImage {
				t.Errorf("image changed: %s → %s", origImage, gotImage)
			}

			t.Logf("After patch: prefill CPU=%s, mem=%s | decode CPU=%s, mem=%s | image=%s",
				gotPrefillCPU.String(), gotPrefillMem.String(),
				gotDecodeCPU.String(), gotDecodeMem.String(), gotImage)
		})
	}
}
