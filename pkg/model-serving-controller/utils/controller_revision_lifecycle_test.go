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

package utils

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestRecordModelServingRevisionLifecycle(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()
	ms := lifecycleTestModelServing()
	dataA := []byte(`{"spec":{"schedulerName":"a","plugins":[],"template":{"roles":[{"name":"role","entryTemplate":{},"workerReplicas":0}]}}}`)
	dataB := []byte(`{"spec":{"schedulerName":"b","plugins":[],"template":{"roles":[{"name":"role","entryTemplate":{},"workerReplicas":0}]}}}`)

	revisionA, collisionCount, err := RecordModelServingRevision(ctx, client, ms, dataA)
	if err != nil {
		t.Fatalf("record A error = %v", err)
	}
	if collisionCount == nil || *collisionCount != 0 {
		t.Fatalf("collisionCount = %v, want 0", collisionCount)
	}
	if revisionA.Revision != 1 {
		t.Fatalf("A.Revision = %d, want 1", revisionA.Revision)
	}
	if got := revisionA.Annotations[ControllerRevisionDataVersionAnnotation]; got != ControllerRevisionDataVersionV1 {
		t.Fatalf("data version = %q, want %q", got, ControllerRevisionDataVersionV1)
	}
	if !bytes.Equal(revisionA.Data.Raw, dataA) {
		t.Fatalf("A.Data = %s, want %s", revisionA.Data.Raw, dataA)
	}

	reusedA, _, err := RecordModelServingRevision(ctx, client, ms, dataA)
	if err != nil {
		t.Fatalf("reuse latest A error = %v", err)
	}
	if reusedA.Name != revisionA.Name || reusedA.Revision != 1 {
		t.Fatalf("reused latest A = %s/%d, want %s/1", reusedA.Name, reusedA.Revision, revisionA.Name)
	}

	revisionB, _, err := RecordModelServingRevision(ctx, client, ms, dataB)
	if err != nil {
		t.Fatalf("record B error = %v", err)
	}
	if revisionB.Revision != 2 {
		t.Fatalf("B.Revision = %d, want 2", revisionB.Revision)
	}

	promotedA, _, err := RecordModelServingRevision(ctx, client, ms, dataA)
	if err != nil {
		t.Fatalf("promote historical A error = %v", err)
	}
	if promotedA.Name != revisionA.Name || promotedA.Revision != 3 {
		t.Fatalf("promoted A = %s/%d, want %s/3", promotedA.Name, promotedA.Revision, revisionA.Name)
	}
	if !bytes.Equal(promotedA.Data.Raw, dataA) {
		t.Fatalf("promoted A data changed: got %s, want %s", promotedA.Data.Raw, dataA)
	}

	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list revisions error = %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("revision count = %d, want 2", len(list.Items))
	}
}

func TestRecordModelServingRevisionResolvesNameCollisionWithoutMutatingData(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()
	ms := lifecycleTestModelServing()
	desiredData := []byte(`{"spec":{"schedulerName":"desired"}}`)
	collidingData := []byte(`{"spec":{"schedulerName":"collision"}}`)
	initialCollisionCount := int32(0)
	unsaltedHash := RevisionDataHash(desiredData, &initialCollisionCount)
	colliding := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GenerateControllerRevisionName(ms.Name, unsaltedHash),
			Namespace: ms.Namespace,
			Labels: map[string]string{
				ControllerRevisionLabelKey:         ms.Name,
				ControllerRevisionRevisionLabelKey: unsaltedHash,
			},
			Annotations: map[string]string{
				ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1,
			},
			OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(ms)},
		},
		Revision: 1,
		Data:     runtime.RawExtension{Raw: collidingData},
	}
	if _, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(ctx, colliding, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create colliding revision error = %v", err)
	}

	created, collisionCount, err := RecordModelServingRevision(ctx, client, ms, desiredData)
	if err != nil {
		t.Fatalf("RecordModelServingRevision() error = %v", err)
	}
	if collisionCount == nil || *collisionCount != 1 {
		t.Fatalf("collisionCount = %v, want 1", collisionCount)
	}
	if created.Name == colliding.Name {
		t.Fatalf("created revision reused colliding name %q", created.Name)
	}
	if want := GenerateControllerRevisionName(ms.Name, RevisionDataHash(desiredData, collisionCount)); created.Name != want {
		t.Fatalf("created name = %q, want %q", created.Name, want)
	}
	if created.Revision != 2 {
		t.Fatalf("created Revision = %d, want 2", created.Revision)
	}

	unchanged, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, colliding.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get colliding revision error = %v", err)
	}
	if !bytes.Equal(unchanged.Data.Raw, collidingData) {
		t.Fatalf("colliding revision data was mutated: got %s, want %s", unchanged.Data.Raw, collidingData)
	}
}

func TestRecordModelServingRevisionIgnoresForeignOwnedHistory(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleTestModelServing()
	data := []byte(`{"spec":{"schedulerName":"desired"}}`)
	foreign := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreign-history",
			Namespace: ms.Namespace,
			Labels:    map[string]string{ControllerRevisionLabelKey: ms.Name},
			OwnerReferences: []metav1.OwnerReference{{
				Controller: ptr.To(true),
				UID:        "old-model-serving-uid",
			}},
		},
		Revision: 99,
		Data:     runtime.RawExtension{Raw: []byte(`{"spec":{"schedulerName":"foreign"}}`)},
	}
	client := kubefake.NewSimpleClientset(foreign)

	created, _, err := RecordModelServingRevision(ctx, client, ms, data)
	if err != nil {
		t.Fatalf("RecordModelServingRevision() error = %v", err)
	}
	if created.Revision != 1 {
		t.Fatalf("created.Revision = %d, want 1", created.Revision)
	}
}

func TestRecordModelServingRevisionDoesNotReuseUnownedCollision(t *testing.T) {
	tests := []struct {
		name        string
		removeOwner bool
	}{
		{name: "foreign owner"},
		{name: "orphan", removeOwner: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleTestModelServing()
			data := []byte(`{"spec":{"schedulerName":"desired"}}`)
			initialCollisionCount := int32(0)
			unowned := revisionForLifecycleTest(
				ms,
				GenerateControllerRevisionName(ms.Name, RevisionDataHash(data, &initialCollisionCount)),
				data,
				99,
			)
			if tt.removeOwner {
				unowned.OwnerReferences = nil
			} else {
				unowned.OwnerReferences[0].UID = "old-model-serving-uid"
			}
			client := kubefake.NewSimpleClientset(unowned)

			created, collisionCount, err := RecordModelServingRevision(ctx, client, ms, data)
			if err != nil {
				t.Fatalf("RecordModelServingRevision() error = %v", err)
			}
			if created.Name == unowned.Name {
				t.Fatalf("reused unowned revision %q", unowned.Name)
			}
			if collisionCount == nil || *collisionCount != 1 {
				t.Fatalf("collisionCount = %v, want 1", collisionCount)
			}
			owner := metav1.GetControllerOf(created)
			if owner == nil || owner.UID != ms.UID {
				t.Fatalf("created revision owner = %#v, want UID %q", owner, ms.UID)
			}
		})
	}
}

func TestRecordModelServingRevisionBreaksRevisionTiesByCreationTime(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleTestModelServing()
	desiredData := []byte(`{"spec":{"schedulerName":"desired"}}`)
	older := revisionForLifecycleTest(ms, "z-older", []byte(`{"spec":{"schedulerName":"old"}}`), 5)
	older.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	newer := revisionForLifecycleTest(ms, "a-newer", desiredData, 5)
	newer.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	client := kubefake.NewSimpleClientset(older, newer)

	got, _, err := RecordModelServingRevision(ctx, client, ms, desiredData)
	if err != nil {
		t.Fatalf("RecordModelServingRevision() error = %v", err)
	}
	if got.Name != newer.Name || got.Revision != 5 {
		t.Fatalf("revision = %s/%d, want latest %s/5", got.Name, got.Revision, newer.Name)
	}
}

func TestUpdateControllerRevisionRetriesConflict(t *testing.T) {
	ctx := context.Background()
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "revision", Namespace: "default"},
		Revision:   1,
	}
	client := kubefake.NewSimpleClientset(revision)
	updateAttempts := 0
	client.PrependReactor("update", "controllerrevisions", func(k8stesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		if updateAttempts == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "apps", Resource: "controllerrevisions"},
				revision.Name,
				fmt.Errorf("concurrent update"),
			)
		}
		return false, nil, nil
	})

	updated, err := updateControllerRevision(ctx, client, revision, 2)
	if err != nil {
		t.Fatalf("updateControllerRevision() error = %v", err)
	}
	if updateAttempts != 2 {
		t.Fatalf("update attempts = %d, want 2", updateAttempts)
	}
	if updated.Revision != 2 {
		t.Fatalf("updated.Revision = %d, want 2", updated.Revision)
	}
}

func TestUpdateControllerRevisionDoesNotDecreaseAfterConflict(t *testing.T) {
	ctx := context.Background()
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "revision", Namespace: "default"},
		Revision:   1,
	}
	client := kubefake.NewSimpleClientset(revision)
	updateAttempts := 0
	client.PrependReactor("update", "controllerrevisions", func(k8stesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		advanced := revision.DeepCopy()
		advanced.Revision = 3
		resource := appsv1.SchemeGroupVersion.WithResource("controllerrevisions")
		if err := client.Tracker().Update(resource, advanced, advanced.Namespace); err != nil {
			t.Fatalf("advance tracked revision: %v", err)
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: "apps", Resource: "controllerrevisions"},
			revision.Name,
			fmt.Errorf("concurrent update"),
		)
	})

	updated, err := updateControllerRevision(ctx, client, revision, 2)
	if err != nil {
		t.Fatalf("updateControllerRevision() error = %v", err)
	}
	if updateAttempts != 1 {
		t.Fatalf("update attempts = %d, want 1", updateAttempts)
	}
	if updated.Revision != 3 {
		t.Fatalf("updated.Revision = %d, want concurrent value 3", updated.Revision)
	}
}

func lifecycleTestModelServing() *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func revisionForLifecycleTest(
	ms *workloadv1alpha1.ModelServing,
	name string,
	data []byte,
	revision int64,
) *appsv1.ControllerRevision {
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ms.Namespace,
			Labels:      map[string]string{ControllerRevisionLabelKey: ms.Name},
			Annotations: map[string]string{ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1},
			OwnerReferences: []metav1.OwnerReference{{
				Controller: ptr.To(true),
				UID:        ms.UID,
			}},
		},
		Revision: revision,
		Data:     runtime.RawExtension{Raw: data},
	}
}
