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

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestScheduleLoraAffinity(t *testing.T) {
	tests := []struct {
		name      string
		config    *conf.RouterConfiguration
		model     string
		isLora    bool
		stickyPod string
		wantNames []string
		wantError bool
	}{
		{name: "default selects loaded adapter", model: "adapter-a", isLora: true, wantNames: []string{"loaded"}},
		{name: "missing adapter fails", model: "adapter-missing", isLora: true, wantError: true},
		{name: "base alias passes all pods", model: "public-base", wantNames: []string{"loaded", "other", "unknown"}},
		{name: "incompatible sticky pod cannot bypass affinity", model: "adapter-a", isLora: true, stickyPod: "other", wantNames: []string{"loaded"}},
		{name: "explicit configuration enables affinity", model: "adapter-a", isLora: true, wantNames: []string{"loaded"},
			config: &conf.RouterConfiguration{Scheduler: conf.SchedulerConfiguration{Plugins: conf.Plugins{
				Filter: conf.Filter{Enabled: []string{plugins.LoraAffinityPluginName}},
			}}}},
		{name: "explicit omission opts out", model: "adapter-a", isLora: true, wantNames: []string{"loaded", "other", "unknown"},
			config: &conf.RouterConfiguration{Scheduler: conf.SchedulerConfiguration{Plugins: conf.Plugins{
				Filter: conf.Filter{Enabled: []string{plugins.LeastRequestPluginName}, Disabled: []string{plugins.LoraAffinityPluginName}},
			}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loaded := createTestPodInfo("loaded")
			loaded.UpdateModels([]string{"base", "adapter-a"})
			other := createTestPodInfo("other")
			other.UpdateModels([]string{"base", "adapter-b"})
			unknown := createTestPodInfo("unknown")
			s := NewScheduler(datastore.New(), tt.config)
			ctx := &framework.Context{Model: tt.model, IsLora: tt.isLora, Prompt: &common.ChatMessage{}, StickyPodName: tt.stickyPod}
			err := s.Schedule(ctx, []*datastore.PodInfo{other, unknown, loaded})
			if tt.wantError {
				require.ErrorContains(t, err, "lora-affinity")
				require.Empty(t, ctx.BestPods)
				return
			}
			require.NoError(t, err)
			names := make([]string, 0, len(ctx.BestPods))
			for _, pod := range ctx.BestPods {
				names = append(names, pod.Pod.Name)
			}
			assert.ElementsMatch(t, tt.wantNames, names)
		})
	}
}

func TestScheduleLoraAffinityPDGroup(t *testing.T) {
	tests := []struct {
		name        string
		missingRole string
		wantError   bool
	}{
		{name: "both roles have adapter"},
		{name: "decode missing adapter", missingRole: "decode", wantError: true},
		{name: "prefill missing adapter", missingRole: "prefill", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := datastore.New()
			msName := types.NamespacedName{Namespace: "default", Name: "lora-pd"}
			ms := &aiv1alpha1.ModelServer{
				ObjectMeta: metav1.ObjectMeta{Name: msName.Name, Namespace: msName.Namespace},
				Spec: aiv1alpha1.ModelServerSpec{WorkloadSelector: &aiv1alpha1.WorkloadSelector{
					PDGroup: &aiv1alpha1.PDGroup{
						GroupKey:      "group",
						DecodeLabels:  map[string]string{"role": "decode"},
						PrefillLabels: map[string]string{"role": "prefill"},
					},
				}},
			}
			require.NoError(t, store.AddOrUpdateModelServer(ms, nil))
			for _, role := range []string{"decode", "prefill"} {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: "default", Labels: map[string]string{"group": "one", "role": role}},
					Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
				}
				require.NoError(t, store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms}))
			}
			pods, err := store.GetPodsByModelServer(msName)
			require.NoError(t, err)
			for _, pod := range pods {
				if pod.Pod.Name != tt.missingRole {
					pod.UpdateModels([]string{"base", "adapter-a"})
				}
			}
			s := NewScheduler(store, nil)
			ctx := &framework.Context{
				Model: "adapter-a", IsLora: true, Prompt: &common.ChatMessage{},
				ModelServerName: msName, PDGroup: ms.Spec.WorkloadSelector.PDGroup,
				StickyPodName: "decode", StickyPrefillPodName: "prefill",
			}
			err = s.Schedule(ctx, pods)
			if tt.wantError {
				require.Error(t, err)
				assert.Empty(t, ctx.DecodePods)
				assert.Empty(t, ctx.PrefillPods)
				return
			}
			require.NoError(t, err)
			require.Len(t, ctx.DecodePods, 1)
			require.Len(t, ctx.PrefillPods, 1)
			assert.Equal(t, "decode", ctx.DecodePods[0].Pod.Name)
			assert.Equal(t, "prefill", ctx.PrefillPods[0].Pod.Name)
		})
	}
}
