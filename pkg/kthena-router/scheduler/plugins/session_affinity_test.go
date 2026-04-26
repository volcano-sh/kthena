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

package plugins

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

func TestSessionAffinityScoreNoSessionKeyIsNoOp(t *testing.T) {
	plugin := NewSessionAffinity(runtime.RawExtension{})
	pods := []*datastore.PodInfo{
		createSessionTestPodInfo("pod-1"),
		createSessionTestPodInfo("pod-2"),
	}

	scores := plugin.Score(&framework.Context{
		AffinityScopeKey: "default/ms-1",
	}, pods)

	require.Len(t, scores, 2)
	for _, score := range scores {
		assert.Equal(t, 0, score)
	}
}

func TestSessionAffinityScoreExistingBindingWins(t *testing.T) {
	plugin := NewSessionAffinity(runtime.RawExtension{})
	pod1 := createSessionTestPodInfo("pod-1")
	pod2 := createSessionTestPodInfo("pod-2")

	plugin.store.set("default/ms-1", "session-1", types.NamespacedName{Namespace: "default", Name: "pod-2"})

	scores := plugin.Score(&framework.Context{
		SessionKey:       "session-1",
		AffinityScopeKey: "default/ms-1",
	}, []*datastore.PodInfo{pod1, pod2})

	assert.Equal(t, 0, scores[pod1])
	assert.Equal(t, maxPluginScore, scores[pod2])
}

func TestSessionAffinityScoreDropsStaleBinding(t *testing.T) {
	plugin := NewSessionAffinity(runtime.RawExtension{})
	pods := []*datastore.PodInfo{
		createSessionTestPodInfo("pod-1"),
		createSessionTestPodInfo("pod-2"),
	}

	plugin.store.set("default/ms-1", "session-1", types.NamespacedName{Namespace: "default", Name: "pod-3"})

	scores := plugin.Score(&framework.Context{
		SessionKey:       "session-1",
		AffinityScopeKey: "default/ms-1",
	}, pods)

	for _, score := range scores {
		assert.Equal(t, 0, score)
	}
	_, ok := plugin.store.get("default/ms-1", "session-1")
	assert.False(t, ok)
}

func TestSessionAffinityPostScheduleStoresBinding(t *testing.T) {
	plugin := NewSessionAffinity(runtime.RawExtension{})
	selectedPod := createSessionTestPodInfo("pod-1")

	plugin.PostSchedule(&framework.Context{
		SessionKey:       "session-1",
		AffinityScopeKey: "default/ms-1",
		BestPods:         []*datastore.PodInfo{selectedPod},
	}, 0)

	binding, ok := plugin.store.get("default/ms-1", "session-1")
	require.True(t, ok)
	assert.Equal(t, types.NamespacedName{Namespace: "default", Name: "pod-1"}, binding)
}

func TestSessionAffinityTTLExpiry(t *testing.T) {
	plugin := NewSessionAffinity(runtime.RawExtension{Raw: []byte("ttl: 1s")})
	now := time.Now()
	plugin.store.now = func() time.Time { return now }
	plugin.store.set("default/ms-1", "session-1", types.NamespacedName{Namespace: "default", Name: "pod-1"})

	now = now.Add(2 * time.Second)

	scores := plugin.Score(&framework.Context{
		SessionKey:       "session-1",
		AffinityScopeKey: "default/ms-1",
	}, []*datastore.PodInfo{createSessionTestPodInfo("pod-1")})

	for _, score := range scores {
		assert.Equal(t, 0, score)
	}
	_, ok := plugin.store.get("default/ms-1", "session-1")
	assert.False(t, ok)
}

func TestSessionAffinityConfigParsesMaxEntries(t *testing.T) {
	cfg := ParseSessionAffinityArgs(runtime.RawExtension{Raw: []byte("maxEntries: 3")})
	assert.Equal(t, 3, cfg.MaxEntries)

	defaultCfg := ParseSessionAffinityArgs(runtime.RawExtension{Raw: []byte("maxEntries: -1")})
	assert.Equal(t, defaultSessionAffinityMaxEntries, defaultCfg.MaxEntries)
}

func TestSessionAffinityConfigParsesPinMode(t *testing.T) {
	cfg := ParseSessionAffinityArgs(runtime.RawExtension{Raw: []byte("pinMode: hard")})
	assert.Equal(t, sessionAffinityPinModeHard, cfg.PinMode)

	defaultCfg := ParseSessionAffinityArgs(runtime.RawExtension{Raw: []byte("pinMode: invalid")})
	assert.Equal(t, sessionAffinityPinModeSoft, defaultCfg.PinMode)
}

func TestSessionAffinityHardPin(t *testing.T) {
	plugin := NewSessionAffinity(runtime.RawExtension{Raw: []byte("pinMode: hard")})
	pod1 := createSessionTestPodInfo("pod-1")
	pod2 := createSessionTestPodInfo("pod-2")

	plugin.store.set("default/ms-1", "session-1", types.NamespacedName{Namespace: "default", Name: "pod-2"})

	pinnedPod, ok := plugin.HardPin(&framework.Context{
		SessionKey:       "session-1",
		AffinityScopeKey: "default/ms-1",
	}, []*datastore.PodInfo{pod1, pod2})

	require.True(t, ok)
	assert.Equal(t, pod2, pinnedPod)

	scores := plugin.Score(&framework.Context{
		SessionKey:       "session-1",
		AffinityScopeKey: "default/ms-1",
	}, []*datastore.PodInfo{pod1, pod2})
	assert.Equal(t, 0, scores[pod1])
	assert.Equal(t, 0, scores[pod2])
}

func TestSessionAffinityStoreEvictsLeastRecentlyUsed(t *testing.T) {
	store := newAffinityStore(30*time.Minute, 2)
	store.set("default/ms-1", "session-1", types.NamespacedName{Namespace: "default", Name: "pod-1"})
	store.set("default/ms-1", "session-2", types.NamespacedName{Namespace: "default", Name: "pod-2"})

	_, ok := store.get("default/ms-1", "session-1")
	require.True(t, ok)

	store.set("default/ms-1", "session-3", types.NamespacedName{Namespace: "default", Name: "pod-3"})

	_, ok = store.get("default/ms-1", "session-2")
	assert.False(t, ok)

	binding, ok := store.get("default/ms-1", "session-1")
	require.True(t, ok)
	assert.Equal(t, "pod-1", binding.Name)

	binding, ok = store.get("default/ms-1", "session-3")
	require.True(t, ok)
	assert.Equal(t, "pod-3", binding.Name)
	assert.Len(t, store.bindings, 2)
}

func TestAffinityBindingKeyUsesSHA256Hash(t *testing.T) {
	key1 := affinityBindingKey("scope-a", "session-1")
	key2 := affinityBindingKey("scope-a", "session-1")
	key3 := affinityBindingKey("scope-a", "session-2")

	assert.Equal(t, key1, key2)
	assert.NotEqual(t, key1, key3)
	assert.NotContains(t, key1, "session-1")

	parts := strings.Split(key1, "\x00")
	require.Len(t, parts, 2)
	assert.Len(t, parts[1], 64)
}

func createSessionTestPodInfo(name string) *datastore.PodInfo {
	return &datastore.PodInfo{
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				PodIP: "10.0.0.1",
			},
		},
	}
}
