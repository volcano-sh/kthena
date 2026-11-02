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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	msplugins "github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	lwslisters "sigs.k8s.io/lws/client-go/listers/leaderworkerset/v1"
)

func TestLWSLegacyChildUpdates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*lwsv1.LeaderWorkerSet)
		allowed bool
		update  bool
	}{
		{"unchanged", func(*lwsv1.LeaderWorkerSet) {}, true, false},
		{"metadata", func(l *lwsv1.LeaderWorkerSet) { l.Annotations = map[string]string{"note": "changed"} }, true, false},
		{"SG shrink", func(l *lwsv1.LeaderWorkerSet) { l.Spec.Replicas = ptr.To[int32](1) }, true, true},
		{"SG zero", func(l *lwsv1.LeaderWorkerSet) { l.Spec.Replicas = ptr.To[int32](0) }, true, true},
		{"SG expand", func(l *lwsv1.LeaderWorkerSet) { l.Spec.Replicas = ptr.To[int32](4) }, false, false},
		{"explicit TCP is equivalent", func(l *lwsv1.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
		}, true, false},
		{"TCP to UDP requires migration", func(l *lwsv1.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP
		}, false, false},
		{"worker shrink", func(l *lwsv1.LeaderWorkerSet) { l.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1) }, false, false},
		{"image plus shrink", func(l *lwsv1.LeaderWorkerSet) {
			l.Spec.Replicas = ptr.To[int32](1)
			l.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "new"
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "parent"}, Spec: lwsv1.LeaderWorkerSetSpec{Replicas: ptr.To[int32](3), LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To[int32](2), WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "old"}}}}}}}
			c := &LWSController{}
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8100}}
			old := c.constructModelServing(lws).DeepCopy()
			old.Spec.Plugins = []workload.PluginSpec{{Name: msplugins.LWSLabelsPluginName, Type: workload.PluginTypeBuiltIn}}
			old.UID = "legacy-child"
			old.ResourceVersion = "1"
			// Simulate the defaults on a stored API object.
			old.Spec.SchedulerName = "volcano"
			old.Spec.RecoveryPolicy = workload.RoleRecreate
			old.Spec.RevisionHistoryLimit = ptr.To[int32](10)
			old.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](0)
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
			old.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
			original := old.DeepCopy()
			c.kthenaClient = kthenafake.NewSimpleClientset(old)
			lwsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			msIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			tc.change(lws)
			require.NoError(t, lwsIndexer.Add(lws))
			require.NoError(t, msIndexer.Add(old))
			c.lwsLister = lwslisters.NewLeaderWorkerSetLister(lwsIndexer)
			c.modelServingLister = workloadlisters.NewModelServingLister(msIndexer)
			err := c.syncHandler(context.Background(), "default/legacy")
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "MigrationRequired")
			}
			current, err := c.kthenaClient.WorkloadV1alpha1().ModelServings("default").Get(context.Background(), "legacy", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, old.UID, current.UID)
			assert.Equal(t, old.Spec.Plugins, current.Spec.Plugins)
			assert.Equal(t, original, old)
			updates := 0
			for _, action := range c.kthenaClient.(*kthenafake.Clientset).Actions() {
				assert.NotEqual(t, "create", action.GetVerb())
				assert.NotEqual(t, "delete", action.GetVerb())
				if action.GetVerb() == "update" {
					updates++
				}
			}
			if tc.update {
				assert.Equal(t, 1, updates)
				assert.Equal(t, *lws.Spec.Replicas, *current.Spec.Replicas)
			} else {
				assert.Zero(t, updates)
				assert.Equal(t, original.Spec, current.Spec)
			}
		})
	}
}
