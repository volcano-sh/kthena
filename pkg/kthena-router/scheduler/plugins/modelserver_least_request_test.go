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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

// addModelServerWithThreshold registers a ModelServer whose least-request
// threshold is either configured (non-nil) or absent (nil).
func addModelServerWithThreshold(t *testing.T, store datastore.Store, name string, threshold *int32) types.NamespacedName {
	t.Helper()
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	if threshold != nil {
		ms.Spec = aiv1alpha1.ModelServerSpec{
			TrafficPolicy: &aiv1alpha1.TrafficPolicy{
				LoadBalancer: &aiv1alpha1.LoadBalancer{
					LeastRequest: &aiv1alpha1.LeastRequest{
						MaxWaitingRequests: threshold,
					},
				},
			},
		}
	}
	require.NoError(t, store.AddOrUpdateModelServer(ms, nil))
	return types.NamespacedName{Namespace: "default", Name: name}
}

func TestModelServerLeastRequestName(t *testing.T) {
	plugin := NewModelServerLeastRequest(datastore.New())
	if plugin.Name() != ModelServerLeastRequestPluginName {
		t.Fatalf("expected plugin name %q, got %q", ModelServerLeastRequestPluginName, plugin.Name())
	}
}

func TestModelServerLeastRequestFilter(t *testing.T) {
	threshold4 := int32(4)

	tests := []struct {
		name           string
		threshold      *int32 // nil = ModelServer exists but the field is unset
		registerServer bool   // false = the ModelServer is unknown to the store
		pods           []*datastore.PodInfo
		wantNames      []string
	}{
		{
			name:           "filters pods at or above the configured threshold",
			threshold:      &threshold4,
			registerServer: true,
			pods: []*datastore.PodInfo{
				makePodWithWaiting("pod-ok", 2),
				makePodWithWaiting("pod-over", 4),   // >= threshold, filtered
				makePodWithWaiting("pod-worse", 10), // > threshold, filtered
			},
			wantNames: []string{"pod-ok"},
		},
		{
			name:           "no threshold configured is a no-op",
			threshold:      nil,
			registerServer: true,
			pods: []*datastore.PodInfo{
				makePodWithWaiting("pod-a", 100),
				makePodWithWaiting("pod-b", 1),
			},
			wantNames: []string{"pod-a", "pod-b"},
		},
		{
			name:           "unknown model server is a no-op",
			threshold:      &threshold4,
			registerServer: false,
			pods: []*datastore.PodInfo{
				makePodWithWaiting("pod-a", 100),
			},
			wantNames: []string{"pod-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := datastore.New()
			msKey := types.NamespacedName{Namespace: "default", Name: "ms-under-test"}
			if tt.registerServer {
				msKey = addModelServerWithThreshold(t, store, "ms-under-test", tt.threshold)
			}

			plugin := NewModelServerLeastRequest(store)
			got := plugin.Filter(&framework.Context{ModelServerName: msKey}, tt.pods)

			gotNames := make([]string, 0, len(got))
			for _, pod := range got {
				gotNames = append(gotNames, pod.Pod.Name)
			}
			assert.ElementsMatch(t, tt.wantNames, gotNames)
		})
	}
}

// TestModelServerLeastRequestPerModelServerIsolation verifies the per-request
// semantics: the threshold is resolved from ctx.ModelServerName, so requests
// targeting different ModelServers are filtered by their own configuration.
func TestModelServerLeastRequestPerModelServerIsolation(t *testing.T) {
	threshold2 := int32(2)
	store := datastore.New()
	strictKey := addModelServerWithThreshold(t, store, "strict-ms", &threshold2)
	looseKey := addModelServerWithThreshold(t, store, "loose-ms", nil) // unset: no-op

	plugin := NewModelServerLeastRequest(store)
	pods := []*datastore.PodInfo{
		makePodWithWaiting("pod-1", 5),
	}

	// Same pod list, different target ModelServer in the context.
	got := plugin.Filter(&framework.Context{ModelServerName: strictKey}, pods)
	assert.Len(t, got, 0, "strict-ms threshold=2 should filter waiting=5")

	got = plugin.Filter(&framework.Context{ModelServerName: looseKey}, pods)
	assert.Len(t, got, 1, "loose-ms has no threshold, pods must pass")
}

// TestModelServerLeastRequestNilStore covers the defensive path when the store
// is not injected: the filter must degrade to a no-op instead of panicking.
func TestModelServerLeastRequestNilStore(t *testing.T) {
	plugin := NewModelServerLeastRequest(nil)
	pods := []*datastore.PodInfo{makePodWithWaiting("pod-1", 100)}

	got := plugin.Filter(&framework.Context{}, pods)
	assert.Len(t, got, 1, "nil store must degrade to a no-op")
}
