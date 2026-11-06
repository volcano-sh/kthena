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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler"
)

// newProfileStatusTestController builds a ProfileStatusController wired to a
// fake kthena clientset and a real ProfileChainManager over the store, with the
// build-settled channel connected.
func newProfileStatusTestController(t *testing.T) (*ProfileStatusController, *kthenafake.Clientset, datastore.Store, *scheduler.ProfileChainManager, informersv1alpha1.SharedInformerFactory, informers.SharedInformerFactory) {
	t.Helper()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	store := newStoreWithMockBackend()
	chainManager := scheduler.NewProfileChainManager(store)
	controller, err := NewProfileStatusController(kthenaInformerFactory, chainManager, chainManager.BuildSettled(), kthenaClient)
	require.NoError(t, err)
	return controller, kthenaClient, store, chainManager, kthenaInformerFactory, kubeInformerFactory
}

// waitForChainBuild waits until the ProfileChainManager settles the profile's build
// outcome: store callbacks run asynchronously (triggerCallbacks spawns a
// goroutine per callback), so a right-after-write BuildStatus query races.
func waitForChainBuild(t *testing.T, chainManager *scheduler.ProfileChainManager, key types.NamespacedName) {
	t.Helper()
	waitForObjectInCache(t, 2*time.Second, func() bool {
		built, buildErr := chainManager.BuildStatus(key)
		return built || buildErr != nil
	})
}

func newModelServerWithProfileRef(namespace, name, profile string) *aiv1alpha1.ModelServer {
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{"app": name},
			},
		},
	}
	if profile != "" {
		ms.Spec.TrafficPolicy = &aiv1alpha1.TrafficPolicy{
			LoadBalancer: &aiv1alpha1.LoadBalancer{SchedulerProfile: &profile},
		}
	}
	return ms
}

func builtProfile(name string) *aiv1alpha1.SchedulerProfile {
	return &aiv1alpha1.SchedulerProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: aiv1alpha1.SchedulerProfileSpec{
			Plugins: aiv1alpha1.Plugins{
				Score: aiv1alpha1.Score{
					Enabled: []aiv1alpha1.PluginWithWeight{{Name: "random", Weight: 1}},
				},
			},
		},
	}
}

// TestProfileStatusController_SchedulerProfileCondition covers the condition
// reported by reportSchedulerProfileCondition for each resolution outcome,
// and that an unchanged condition is not written again.
func TestProfileStatusController_SchedulerProfileCondition(t *testing.T) {
	invalidProfile := &aiv1alpha1.SchedulerProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1"},
		Spec: aiv1alpha1.SchedulerProfileSpec{
			PluginConfig: []aiv1alpha1.PluginConfig{{Name: "no-such-plugin"}},
		},
	}

	tests := []struct {
		name       string
		profile    *aiv1alpha1.SchedulerProfile
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "profile not found",
			profile:    nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: aiv1alpha1.SchedulerProfileResolvedReasonNotFound,
		},
		{
			name:       "profile build failed",
			profile:    invalidProfile,
			wantStatus: metav1.ConditionFalse,
			wantReason: aiv1alpha1.SchedulerProfileResolvedReasonResolutionFailed,
		},
		{
			name:       "profile resolved",
			profile:    builtProfile("p1"),
			wantStatus: metav1.ConditionTrue,
			wantReason: aiv1alpha1.SchedulerProfileResolvedReasonResolved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, kthenaClient, store, chainManager, _, _ := newProfileStatusTestController(t)

			ms := newModelServerWithProfileRef("default", "ms", "p1")
			_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
				context.Background(), ms, metav1.CreateOptions{})
			require.NoError(t, err)

			if tt.profile != nil {
				store.AddOrUpdateSchedulerProfile(tt.profile)
				waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})
			}

			controller.reportSchedulerProfileCondition(ms)

			got, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
				context.Background(), "ms", metav1.GetOptions{})
			require.NoError(t, err)
			require.Len(t, got.Status.Conditions, 1)
			cond := got.Status.Conditions[0]
			assert.Equal(t, aiv1alpha1.SchedulerProfileResolvedConditionType, cond.Type)
			assert.Equal(t, tt.wantStatus, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)

			// Re-reporting an unchanged condition (what the lister serves once
			// the watch delivers the update) must not write again.
			kthenaClient.ClearActions()
			controller.reportSchedulerProfileCondition(got)
			assert.Empty(t, kthenaClient.Actions())
		})
	}

	t.Run("no profile reference", func(t *testing.T) {
		controller, kthenaClient, _, _, _, _ := newProfileStatusTestController(t)

		ms := newModelServerWithProfileRef("default", "ms", "")
		_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		require.NoError(t, err)
		kthenaClient.ClearActions()

		controller.reportSchedulerProfileCondition(ms)

		assert.Empty(t, kthenaClient.Actions())
	})

	t.Run("profile exists but chain not built yet", func(t *testing.T) {
		controller, kthenaClient, _, _, kthenaInformerFactory, _ := newProfileStatusTestController(t)

		// The profile is visible to the lister but was never added to the
		// store, so the ProfileChainManager has not built (or finished building)
		// its chain — the async store callback may still be in flight.
		require.NoError(t, kthenaInformerFactory.Networking().V1alpha1().SchedulerProfiles().
			Informer().GetIndexer().Add(&aiv1alpha1.SchedulerProfile{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1"},
		}))

		ms := newModelServerWithProfileRef("default", "ms", "p1")
		_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		require.NoError(t, err)
		kthenaClient.ClearActions()

		controller.reportSchedulerProfileCondition(ms)

		// In-flight, not missing: no condition write; the build-settled
		// signal re-triggers reporting.
		assert.Empty(t, kthenaClient.Actions())
	})
}

// TestProfileStatusController_ReportsResolvedOnSync verifies the full Run
// flow: an existing ModelServer referencing a built profile gets its
// condition written by the controller's own event path.
func TestProfileStatusController_ReportsResolvedOnSync(t *testing.T) {
	controller, kthenaClient, store, chainManager, kthenaInformerFactory, kubeInformerFactory := newProfileStatusTestController(t)

	ms := newModelServerWithProfileRef("default", "ms", "p1")
	_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
		context.Background(), ms, metav1.CreateOptions{})
	require.NoError(t, err)
	store.AddOrUpdateSchedulerProfile(builtProfile("p1"))
	waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)
	go func() { _ = controller.Run(stop) }()

	assert.Eventually(t, func() bool {
		got, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
			context.Background(), "ms", metav1.GetOptions{})
		if err != nil || len(got.Status.Conditions) != 1 {
			return false
		}
		cond := got.Status.Conditions[0]
		return cond.Type == aiv1alpha1.SchedulerProfileResolvedConditionType &&
			cond.Status == metav1.ConditionTrue &&
			cond.Reason == aiv1alpha1.SchedulerProfileResolvedReasonResolved
	}, 5*time.Second, 50*time.Millisecond, "condition must be reported through the controller's own sync path")
}

// TestProfileStatusController_BuildSettledConverges verifies the closed loop:
// a ModelServer referencing a missing profile first reports NotFound, and the
// build-settled signal flips it to Resolved once the profile lands — with no
// other event in between.
func TestProfileStatusController_BuildSettledConverges(t *testing.T) {
	controller, kthenaClient, store, chainManager, kthenaInformerFactory, kubeInformerFactory := newProfileStatusTestController(t)

	ms := newModelServerWithProfileRef("default", "ms", "p1")
	_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
		context.Background(), ms, metav1.CreateOptions{})
	require.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)
	go func() { _ = controller.Run(stop) }()

	// Step 1: profile does not exist yet — NotFound is reported.
	assert.Eventually(t, func() bool {
		got, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
			context.Background(), "ms", metav1.GetOptions{})
		return err == nil && len(got.Status.Conditions) == 1 &&
			got.Status.Conditions[0].Reason == aiv1alpha1.SchedulerProfileResolvedReasonNotFound
	}, 5*time.Second, 50*time.Millisecond, "missing profile must report NotFound")

	// Step 2: the profile lands and the chain settles; the settled signal
	// re-enqueues the referencing ModelServer, which now reports Resolved.
	_, err = kthenaClient.NetworkingV1alpha1().SchedulerProfiles("default").Create(
		context.Background(), builtProfile("p1"), metav1.CreateOptions{})
	require.NoError(t, err)
	store.AddOrUpdateSchedulerProfile(builtProfile("p1"))
	waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})

	assert.Eventually(t, func() bool {
		got, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
			context.Background(), "ms", metav1.GetOptions{})
		return err == nil && len(got.Status.Conditions) == 1 &&
			got.Status.Conditions[0].Reason == aiv1alpha1.SchedulerProfileResolvedReasonResolved
	}, 5*time.Second, 50*time.Millisecond, "build settle must converge the condition to Resolved")
}

// TestProfileStatusController_EnqueueModelServersReferencing verifies a
// SchedulerProfile event enqueues exactly the ModelServers referencing it,
// including through delete-event tombstones.
func TestProfileStatusController_EnqueueModelServersReferencing(t *testing.T) {
	controller, _, _, _, kthenaInformerFactory, _ := newProfileStatusTestController(t)

	indexer := kthenaInformerFactory.Networking().V1alpha1().ModelServers().Informer().GetIndexer()
	require.NoError(t, indexer.Add(newModelServerWithProfileRef("default", "ms-ref", "p1")))
	require.NoError(t, indexer.Add(newModelServerWithProfileRef("default", "ms-other", "p2")))
	require.NoError(t, indexer.Add(newModelServerWithProfileRef("default", "ms-plain", "")))

	profile := &aiv1alpha1.SchedulerProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1"},
	}

	controller.enqueueModelServersReferencing(profile)
	assert.Equal(t, 1, controller.workqueue.Len())
	obj, _ := controller.workqueue.Get()
	controller.workqueue.Done(obj)
	controller.workqueue.Forget(obj)
	assert.Equal(t, "default/ms-ref", obj.(string))

	// Delete events deliver the object wrapped in a tombstone.
	controller.enqueueModelServersReferencing(cache.DeletedFinalStateUnknown{
		Key: "default/p1",
		Obj: profile,
	})
	assert.Equal(t, 1, controller.workqueue.Len())
	obj, _ = controller.workqueue.Get()
	controller.workqueue.Done(obj)
	controller.workqueue.Forget(obj)
	assert.Equal(t, "default/ms-ref", obj.(string))
}

// TestProfileStatusController_SyncHandlerEdges covers the sync paths that
// report nothing: a deleted ModelServer, an invalid queue key, and a
// queue item that is not a key at all.
func TestProfileStatusController_SyncHandlerEdges(t *testing.T) {
	controller, kthenaClient, _, _, _, _ := newProfileStatusTestController(t)

	controller.syncHandler("default/gone") // ModelServer not found
	controller.syncHandler("bad-key")      // invalid key
	assert.Empty(t, kthenaClient.Actions())

	controller.workqueue.Add(42) // not a string key
	assert.True(t, controller.processNextWorkItem())
	assert.Empty(t, kthenaClient.Actions())
}

// TestProfileStatusController_HasSynced covers the readiness flag surfaced
// to the router's startup gate.
func TestProfileStatusController_HasSynced(t *testing.T) {
	controller, _, _, _, _, _ := newProfileStatusTestController(t)

	assert.False(t, controller.HasSynced())
	controller.initialSync.Store(true)
	assert.True(t, controller.HasSynced())
}

// TestProfileStatusController_ReferenceRemovedClearsCondition verifies that
// removing the SchedulerProfile reference clears the condition (the
// ModelServer falls back to the global chain) instead of leaving a stale one.
func TestProfileStatusController_ReferenceRemovedClearsCondition(t *testing.T) {
	controller, kthenaClient, store, chainManager, _, _ := newProfileStatusTestController(t)

	ms := newModelServerWithProfileRef("default", "ms", "p1")
	_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
		context.Background(), ms, metav1.CreateOptions{})
	require.NoError(t, err)
	store.AddOrUpdateSchedulerProfile(builtProfile("p1"))
	waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})

	controller.reportSchedulerProfileCondition(ms)
	got, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
		context.Background(), "ms", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Status.Conditions, 1)

	// Remove the reference and re-report: the condition must disappear.
	_, err = kthenaClient.NetworkingV1alpha1().ModelServers("default").Update(
		context.Background(), newModelServerWithProfileRef("default", "ms", ""), metav1.UpdateOptions{})
	require.NoError(t, err)
	got, err = kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
		context.Background(), "ms", metav1.GetOptions{})
	require.NoError(t, err)
	controller.reportSchedulerProfileCondition(got)

	got, err = kthenaClient.NetworkingV1alpha1().ModelServers("default").Get(
		context.Background(), "ms", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, got.Status.Conditions, "the condition must be cleared once the reference is removed")

	// Reporting again (no condition, no reference) must be a silent no-op.
	kthenaClient.ClearActions()
	controller.reportSchedulerProfileCondition(got)
	assert.Empty(t, kthenaClient.Actions())
}

// TestProfileStatusController_FailedWriteIsReturned verifies a failed status
// write is surfaced so the workqueue retries it, while a NotFound (the object
// is gone) is swallowed.
func TestProfileStatusController_FailedWriteIsReturned(t *testing.T) {
	t.Run("transient error is retried", func(t *testing.T) {
		controller, kthenaClient, store, chainManager, _, _ := newProfileStatusTestController(t)

		ms := newModelServerWithProfileRef("default", "ms", "p1")
		_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		require.NoError(t, err)

		store.AddOrUpdateSchedulerProfile(builtProfile("p1"))
		waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})

		kthenaClient.PrependReactor("update", "modelservers", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
		})

		require.Error(t, controller.reportSchedulerProfileCondition(ms),
			"a failed status write must be returned so the workqueue retries")
	})

	t.Run("not found is not retried", func(t *testing.T) {
		controller, kthenaClient, store, chainManager, _, _ := newProfileStatusTestController(t)

		ms := newModelServerWithProfileRef("default", "ms", "p1")
		_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		require.NoError(t, err)

		store.AddOrUpdateSchedulerProfile(builtProfile("p1"))
		waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})

		kthenaClient.PrependReactor("update", "modelservers", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewNotFound(aiv1alpha1.Resource("modelservers"), "ms")
		})

		require.NoError(t, controller.reportSchedulerProfileCondition(ms),
			"a ModelServer that is already gone needs no retry")
	})

	t.Run("clear failure is retried", func(t *testing.T) {
		controller, kthenaClient, _, _, _, _ := newProfileStatusTestController(t)

		// A ModelServer that still carries the condition but no longer
		// references a profile goes through the clear path.
		ms := newModelServerWithProfileRef("default", "ms", "")
		ms.Status.Conditions = []metav1.Condition{{
			Type:   aiv1alpha1.SchedulerProfileResolvedConditionType,
			Status: metav1.ConditionTrue,
			Reason: aiv1alpha1.SchedulerProfileResolvedReasonResolved,
		}}
		_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		require.NoError(t, err)

		kthenaClient.PrependReactor("update", "modelservers", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
		})

		require.Error(t, controller.reportSchedulerProfileCondition(ms),
			"a failed clear must be returned so the workqueue retries")
	})
}

// TestProfileStatusController_FailedWriteIsRequeued verifies a failed status
// write puts the key back on the workqueue, so the condition converges without
// waiting for an unrelated event.
func TestProfileStatusController_FailedWriteIsRequeued(t *testing.T) {
	controller, kthenaClient, store, chainManager, kthenaInformerFactory, _ := newProfileStatusTestController(t)

	ms := newModelServerWithProfileRef("default", "ms", "p1")
	require.NoError(t, kthenaInformerFactory.Networking().V1alpha1().ModelServers().Informer().GetIndexer().Add(ms))

	store.AddOrUpdateSchedulerProfile(builtProfile("p1"))
	waitForChainBuild(t, chainManager, types.NamespacedName{Namespace: "default", Name: "p1"})

	kthenaClient.PrependReactor("update", "modelservers", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	const key = "default/ms"
	controller.workqueue.Add(key)
	require.True(t, controller.processNextWorkItem())

	assert.Equal(t, 1, controller.workqueue.NumRequeues(key),
		"a failed write must requeue the key so the condition converges on retry")
}
