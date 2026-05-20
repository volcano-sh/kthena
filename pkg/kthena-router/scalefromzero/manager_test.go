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

package scalefromzero

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type fakeBindingLister struct {
	bindings []*workloadv1alpha1.AutoscalingPolicyBinding
	err      error
}

func (f *fakeBindingLister) List(selector labels.Selector) ([]*workloadv1alpha1.AutoscalingPolicyBinding, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bindings, nil
}

func (f *fakeBindingLister) AutoscalingPolicyBindings(namespace string) listerv1alpha1.AutoscalingPolicyBindingNamespaceLister {
	return &fakeBindingNamespaceLister{}
}

type fakeBindingNamespaceLister struct{}

func (f *fakeBindingNamespaceLister) List(selector labels.Selector) ([]*workloadv1alpha1.AutoscalingPolicyBinding, error) {
	return nil, nil
}

func (f *fakeBindingNamespaceLister) Get(name string) (*workloadv1alpha1.AutoscalingPolicyBinding, error) {
	return nil, nil
}

func newTestSetup(t *testing.T, servingName, msName string) (*Manager, *kthenafake.Clientset) {
	t.Helper()
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: servingName},
	}
	client := kthenafake.NewSimpleClientset(ms)

	binding := &workloadv1alpha1.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-binding"},
		Spec: workloadv1alpha1.AutoscalingPolicyBindingSpec{
			PolicyRef: corev1.LocalObjectReference{Name: "test-policy"},
			HomogeneousTarget: &workloadv1alpha1.HomogeneousTarget{
				Target: workloadv1alpha1.Target{
					TargetRef: corev1.ObjectReference{Kind: "ModelServing", Name: servingName, Namespace: "default"},
				},
				MinReplicas: 0, MaxReplicas: 10,
			},
		},
	}
	lister := &fakeBindingLister{bindings: []*workloadv1alpha1.AutoscalingPolicyBinding{binding}}

	m := NewManager(client, lister)
	t.Cleanup(m.Stop)

	m.SetModelServerMapping("default", msName, servingName)
	return m, client
}

func TestNewManager(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		m := NewManager(nil, nil)
		assert.False(t, m.Enabled())
	})

	t.Run("enabled via env", func(t *testing.T) {
		t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
		m := NewManager(nil, &fakeBindingLister{})
		assert.True(t, m.Enabled())
		m.Stop()
	})

	t.Run("custom timeout via env", func(t *testing.T) {
		t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
		t.Setenv("SCALE_FROM_ZERO_TIMEOUT", "30s")
		m := NewManager(nil, &fakeBindingLister{})
		assert.Equal(t, 30*time.Second, m.timeout)
		m.Stop()
	})
}

func TestHandle_NotEnabled(t *testing.T) {
	m := NewManager(nil, nil)
	_, _, err := m.Handle(context.Background(), "default", "test-ms")
	assert.ErrorIs(t, err, ErrNotEnabled)
}

func TestHandle_NoMapping(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, &fakeBindingLister{})
	defer m.Stop()

	_, _, err := m.Handle(context.Background(), "default", "test-ms")
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestHandle_NoBinding(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	lister := &fakeBindingLister{bindings: []*workloadv1alpha1.AutoscalingPolicyBinding{}}
	m := NewManager(nil, lister)
	defer m.Stop()

	m.SetModelServerMapping("default", "test-ms", "test-serving")

	_, _, err := m.Handle(context.Background(), "default", "test-ms")
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestHandle_HasBinding_TriggersScaleUp(t *testing.T) {
	m, client := newTestSetup(t, "test-serving", "test-ms")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := m.Handle(ctx, "default", "test-ms")
	assert.ErrorIs(t, err, ErrTimeout)

	patched, err := client.WorkloadV1alpha1().ModelServings("default").Get(context.Background(), "test-serving", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, int32(1), *patched.Spec.Replicas)
}

func TestHandle_HasBinding_WaitForPods(t *testing.T) {
	m, _ := newTestSetup(t, "test-serving", "test-ms")

	handleDone := make(chan struct{})
	var resultPods []*datastore.PodInfo
	var resultMS *networkingv1alpha1.ModelServer
	var handleErr error
	go func() {
		defer close(handleDone)
		resultPods, resultMS, handleErr = m.Handle(context.Background(), "default", "test-ms")
	}()

	time.Sleep(50 * time.Millisecond)
	modelServer := &networkingv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms"},
	}
	m.OnPodsAvailable("default", "test-ms", []*datastore.PodInfo{{Pod: &corev1.Pod{}}}, modelServer)

	select {
	case <-handleDone:
		assert.NoError(t, handleErr)
		assert.Len(t, resultPods, 1)
		assert.Equal(t, "test-ms", resultMS.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not complete after pods became available")
	}
}

func TestHandle_HasBinding_Timeout(t *testing.T) {
	t.Setenv("SCALE_FROM_ZERO_TIMEOUT", "50ms")
	m, _ := newTestSetup(t, "test-serving", "test-ms")

	_, _, err := m.Handle(context.Background(), "default", "test-ms")
	assert.ErrorIs(t, err, ErrTimeout)
}

func TestHandle_ConcurrentRequestsShareTrigger(t *testing.T) {
	m, _ := newTestSetup(t, "test-serving", "test-ms")

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			m.Handle(ctx, "default", "test-ms")
		}()
	}

	time.Sleep(50 * time.Millisecond)

	m.pendingMu.Lock()
	assert.Len(t, m.pending, 1)
	m.pendingMu.Unlock()

	modelServer := &networkingv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms"},
	}
	m.OnPodsAvailable("default", "test-ms", []*datastore.PodInfo{{Pod: &corev1.Pod{}}}, modelServer)

	wg.Wait()
}

func TestOnPodsAvailable_NoPending(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, &fakeBindingLister{})
	defer m.Stop()

	modelServer := &networkingv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms"},
	}
	m.OnPodsAvailable("default", "test-ms", []*datastore.PodInfo{{Pod: &corev1.Pod{}}}, modelServer)
}

func TestSetDeleteModelServerMapping(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, &fakeBindingLister{})
	defer m.Stop()

	m.SetModelServerMapping("default", "ms1", "serving1")

	m.mu.RLock()
	val, ok := m.servingForMS["default/ms1"]
	m.mu.RUnlock()
	assert.True(t, ok)
	assert.Equal(t, "default/serving1", val)

	m.DeleteModelServerMapping("default", "ms1")

	m.mu.RLock()
	_, ok = m.servingForMS["default/ms1"]
	m.mu.RUnlock()
	assert.False(t, ok)
}

func TestDeleteModelServerMapping_ClearsPending(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, &fakeBindingLister{})
	defer m.Stop()

	m.pendingMu.Lock()
	m.pending["default/ms1"] = &pendingWait{
		resultChan: make(chan *waitResult, 1),
	}
	m.pendingMu.Unlock()

	m.DeleteModelServerMapping("default", "ms1")

	m.pendingMu.Lock()
	_, ok := m.pending["default/ms1"]
	m.pendingMu.Unlock()
	assert.False(t, ok)
}

func TestBindingCacheRefresh(t *testing.T) {
	binding := &workloadv1alpha1.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-binding",
		},
		Spec: workloadv1alpha1.AutoscalingPolicyBindingSpec{
			PolicyRef: corev1.LocalObjectReference{Name: "test-policy"},
			HomogeneousTarget: &workloadv1alpha1.HomogeneousTarget{
				Target: workloadv1alpha1.Target{
					TargetRef: corev1.ObjectReference{
						Kind: "ModelServing",
						Name: "test-serving",
					},
				},
				MinReplicas: 0,
				MaxReplicas: 10,
			},
		},
	}
	lister := &fakeBindingLister{bindings: []*workloadv1alpha1.AutoscalingPolicyBinding{binding}}

	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, lister)
	defer m.Stop()

	time.Sleep(50 * time.Millisecond)

	assert.True(t, m.hasAutoscalingPolicy("default/test-serving"))
	assert.False(t, m.hasAutoscalingPolicy("default/other-serving"))
}

func TestBindingCacheRefresh_HeterogeneousTarget(t *testing.T) {
	binding := &workloadv1alpha1.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-binding",
		},
		Spec: workloadv1alpha1.AutoscalingPolicyBindingSpec{
			PolicyRef: corev1.LocalObjectReference{Name: "test-policy"},
			HeterogeneousTarget: &workloadv1alpha1.HeterogeneousTarget{
				Params: []workloadv1alpha1.HeterogeneousTargetParam{
					{
						Target: workloadv1alpha1.Target{
							TargetRef: corev1.ObjectReference{
								Kind: "ModelServing",
								Name: "hetero-serving",
							},
						},
						MinReplicas: 0,
						MaxReplicas: 10,
					},
				},
			},
		},
	}
	lister := &fakeBindingLister{bindings: []*workloadv1alpha1.AutoscalingPolicyBinding{binding}}

	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, lister)
	defer m.Stop()

	time.Sleep(50 * time.Millisecond)

	assert.True(t, m.hasAutoscalingPolicy("default/hetero-serving"))
}

func TestBindingCache_WithNilLister(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, nil)
	defer m.Stop()

	assert.False(t, m.hasAutoscalingPolicy("default/test-serving"))
}

func TestBindingCache_ListError(t *testing.T) {
	lister := &fakeBindingLister{err: errors.New("list error")}

	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, lister)
	defer m.Stop()

	assert.False(t, m.hasAutoscalingPolicy("default/test-serving"))
}

func TestTriggerScaleUp_InvalidKey(t *testing.T) {
	t.Setenv("ENABLE_SCALE_FROM_ZERO", "true")
	m := NewManager(nil, &fakeBindingLister{})
	defer m.Stop()

	resultChan := make(chan *waitResult, 1)
	m.pendingMu.Lock()
	m.pending["default/ms1"] = &pendingWait{
		resultChan: resultChan,
	}
	m.pendingMu.Unlock()

	m.triggerScaleUp("invalid-key", "default/ms1")

	select {
	case result := <-resultChan:
		assert.ErrorIs(t, result.err, ErrTriggerFailed)
	case <-time.After(time.Second):
		t.Fatal("expected error notification")
	}
}

func TestGetEnvBool(t *testing.T) {
	assert.False(t, getEnvBool("NONEXISTENT_VAR", false))
	assert.True(t, getEnvBool("NONEXISTENT_VAR", true))

	t.Run("true", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "true")
		assert.True(t, getEnvBool("TEST_BOOL", false))
	})
	t.Run("false", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "false")
		assert.False(t, getEnvBool("TEST_BOOL", true))
	})
	t.Run("invalid", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "invalid")
		assert.False(t, getEnvBool("TEST_BOOL", false))
	})
}

func TestParseDurationEnv(t *testing.T) {
	assert.Equal(t, 5*time.Minute, parseDurationEnv("NONEXISTENT", 5*time.Minute))

	t.Run("valid", func(t *testing.T) {
		t.Setenv("TEST_DURATION", "30s")
		assert.Equal(t, 30*time.Second, parseDurationEnv("TEST_DURATION", time.Hour))
	})
	t.Run("invalid", func(t *testing.T) {
		t.Setenv("TEST_DURATION", "invalid")
		assert.Equal(t, time.Hour, parseDurationEnv("TEST_DURATION", time.Hour))
	})
}
