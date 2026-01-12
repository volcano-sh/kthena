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

package util

import (
	"context"
	"testing"
	"time"

	clientsetfake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestGetRoleName(t *testing.T) {
	tests := []struct {
		name     string
		refName  string
		wantRole string
		wantSub  string
		wantErr  bool
	}{
		{
			name:     "valid role name",
			refName:  "role/sub",
			wantRole: "role",
			wantSub:  "sub",
			wantErr:  false,
		},
		{
			name:     "invalid role name - no separator",
			refName:  "invalid",
			wantRole: "",
			wantSub:  "",
			wantErr:  true,
		},
		{
			name:     "valid role name with multiple parts",
			refName:  "admin/user",
			wantRole: "admin",
			wantSub:  "user",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := &corev1.ObjectReference{Name: tt.refName}
			role, sub, err := GetRoleName(ref)

			if (err != nil) != tt.wantErr {
				t.Errorf("GetRoleName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if role != tt.wantRole {
					t.Errorf("GetRoleName() role = %v, want %v", role, tt.wantRole)
				}
				if sub != tt.wantSub {
					t.Errorf("GetRoleName() sub = %v, want %v", sub, tt.wantSub)
				}
			}
		})
	}
}

func TestGetTargetLabels(t *testing.T) {
	tests := []struct {
		name       string
		targetName string
		targetKind string
		wantErr    bool
		wantNil    bool
	}{
		{
			name:       "valid model serving target",
			targetName: "model1",
			targetKind: workload.ModelServingKind.Kind,
			wantErr:    false,
			wantNil:    false,
		},
		{
			name:       "another valid target",
			targetName: "model2",
			targetKind: workload.ModelServingKind.Kind,
			wantErr:    false,
			wantNil:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &workload.Target{}
			target.TargetRef.Name = tt.targetName
			target.TargetRef.Kind = tt.targetKind

			selector, err := GetTargetLabels(target)

			if (err != nil) != tt.wantErr {
				t.Errorf("GetTargetLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if (selector == nil) != tt.wantNil {
				t.Errorf("GetTargetLabels() selector nil = %v, wantNil %v", selector == nil, tt.wantNil)
			}
		})
	}
}

func TestGetMetricPods(t *testing.T) {
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			Labels: map[string]string{
				workload.ModelServingNameLabelKey: "model1",
				workload.EntryLabelKey:            Entry,
			},
		},
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod2",
			Namespace: "other-namespace",
			Labels: map[string]string{
				workload.ModelServingNameLabelKey: "model1",
				workload.EntryLabelKey:            Entry,
			},
		},
	}

	// Setup fake client and informers
	kubeClient := kubefake.NewSimpleClientset(pod1, pod2)
	informerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	podLister := informerFactory.Core().V1().Pods().Lister()

	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	tests := []struct {
		name         string
		namespace    string
		targetName   string
		wantPodCount int
		wantPodNames []string
		wantErr      bool
	}{
		{
			name:         "pods in default namespace",
			namespace:    "default",
			targetName:   "model1",
			wantPodCount: 1,
			wantPodNames: []string{"pod1"},
			wantErr:      false,
		},
		{
			name:         "pods in other-namespace",
			namespace:    "other-namespace",
			targetName:   "model1",
			wantPodCount: 1,
			wantPodNames: []string{"pod2"},
			wantErr:      false,
		},
		{
			name:         "pods in non-existent namespace",
			namespace:    "non-existent",
			targetName:   "model1",
			wantPodCount: 0,
			wantPodNames: []string{},
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &workload.Target{}
			target.TargetRef.Name = tt.targetName
			target.TargetRef.Kind = workload.ModelServingKind.Kind

			pods, err := GetMetricPods(podLister, tt.namespace, target)

			if (err != nil) != tt.wantErr {
				t.Errorf("GetMetricPods() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(pods) != tt.wantPodCount {
				t.Errorf("GetMetricPods() got %d pods, want %d", len(pods), tt.wantPodCount)
				return
			}

			// Verify pod names
			for i, wantName := range tt.wantPodNames {
				if i >= len(pods) {
					t.Errorf("GetMetricPods() missing pod at index %d", i)
					continue
				}
				if pods[i].Name != wantName {
					t.Errorf("GetMetricPods() pod[%d].Name = %s, want %s", i, pods[i].Name, wantName)
				}
			}
		})
	}
}

func TestUpdateModelServing(t *testing.T) {
	tests := []struct {
		name           string
		modelName      string
		modelNamespace string
		wantErr        bool
	}{
		{
			name:           "successful update",
			modelName:      "model1",
			modelNamespace: "default",
			wantErr:        false,
		},
		{
			name:           "update another model",
			modelName:      "model2",
			modelNamespace: "kube-system",
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset()

			model := &workload.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.modelName,
					Namespace: tt.modelNamespace,
				},
			}

			_, err := client.WorkloadV1alpha1().
				ModelServings(tt.modelNamespace).
				Create(context.Background(), model, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err = UpdateModelServing(ctx, client, model)

			if (err != nil) != tt.wantErr {
				t.Errorf("UpdateModelServing() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetModelServingTarget(t *testing.T) {
	tests := []struct {
		name          string
		modelName     string
		namespace     string
		lookupName    string
		lookupNs      string
		wantErr       bool
		wantModelName string
	}{
		{
			name:          "existing model serving",
			modelName:     "model1",
			namespace:     "default",
			lookupName:    "model1",
			lookupNs:      "default",
			wantErr:       false,
			wantModelName: "model1",
		},
		{
			name:       "non-existent model serving",
			modelName:  "model1",
			namespace:  "default",
			lookupName: "model2",
			lookupNs:   "default",
			wantErr:    true,
		},
		{
			name:       "wrong namespace",
			modelName:  "model1",
			namespace:  "default",
			lookupName: "model1",
			lookupNs:   "other",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			model := &workload.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.modelName,
					Namespace: tt.namespace,
				},
			}
			indexer.Add(model)

			lister := workloadlisters.NewModelServingLister(indexer)

			result, err := GetModelServingTarget(lister, tt.lookupNs, tt.lookupName)

			if (err != nil) != tt.wantErr {
				t.Errorf("GetModelServingTarget() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && result.Name != tt.wantModelName {
				t.Errorf("GetModelServingTarget() name = %v, want %v", result.Name, tt.wantModelName)
			}
		})
	}
}
