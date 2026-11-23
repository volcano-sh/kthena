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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func newTestSchedulerProfile(name string) *aiv1alpha1.SchedulerProfile {
	return &aiv1alpha1.SchedulerProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
	}
}

// newTestSchedulerProfileController builds a controller whose informer cache
// is started and synced. Profiles created in the fake clientset before this
// call are visible to the lister.
func newTestSchedulerProfileController(t *testing.T, initObjs ...*aiv1alpha1.SchedulerProfile) (*SchedulerProfileController, datastore.Store) {
	t.Helper()
	kthenaClient := kthenafake.NewSimpleClientset()
	for _, obj := range initObjs {
		_, err := kthenaClient.NetworkingV1alpha1().SchedulerProfiles(obj.Namespace).
			Create(context.Background(), obj, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	store := newStoreWithMockBackend()

	controller, err := NewSchedulerProfileController(kthenaInformerFactory, store)
	require.NoError(t, err)

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	kthenaInformerFactory.Start(stopCh)
	kthenaInformerFactory.WaitForCacheSync(stopCh)

	return controller, store
}

func TestSchedulerProfileController_SyncHandler(t *testing.T) {
	tests := []struct {
		name       string
		initObjs   []*aiv1alpha1.SchedulerProfile
		key        string
		wantExists bool
	}{
		{
			name:       "profile is added to the store",
			initObjs:   []*aiv1alpha1.SchedulerProfile{newTestSchedulerProfile("p1")},
			key:        "default/p1",
			wantExists: true,
		},
		{
			name:       "deleted profile is removed from the store",
			initObjs:   nil,
			key:        "default/p1",
			wantExists: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, store := newTestSchedulerProfileController(t, tt.initObjs...)
			if !tt.wantExists {
				// The deleted case starts with the profile already in the store.
				store.AddOrUpdateSchedulerProfile(newTestSchedulerProfile("p1"))
			}

			assert.NoError(t, c.syncHandler(tt.key))

			got := store.GetSchedulerProfile(types.NamespacedName{Namespace: "default", Name: "p1"})
			if tt.wantExists {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

func TestSchedulerProfileController_HasSynced(t *testing.T) {
	c, _ := newTestSchedulerProfileController(t)
	assert.False(t, c.HasSynced())

	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		_ = c.Run(stopCh)
	}()

	// HasSynced flips only after the worker drains the initialSync signal,
	// i.e. the initial batch of profiles has landed in the store.
	assert.Eventually(t, c.HasSynced, 5*time.Second, 50*time.Millisecond)
}

func TestSchedulerProfileController_RunFillsStore(t *testing.T) {
	c, store := newTestSchedulerProfileController(t, newTestSchedulerProfile("p1"))

	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		_ = c.Run(stopCh)
	}()

	// The Run flow must fill the store with the existing profiles.
	assert.Eventually(t, func() bool {
		return store.GetSchedulerProfile(types.NamespacedName{Namespace: "default", Name: "p1"}) != nil
	}, 5*time.Second, 50*time.Millisecond)
}
