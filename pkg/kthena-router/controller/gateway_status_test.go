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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestUpdateGatewayStatus_AcceptedAndProgrammed(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	informerFactory := gatewayinformers.NewSharedInformerFactory(client, 0)
	store := datastore.New()
	ctrl, err := NewGatewayController(informerFactory, store, client)
	require.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	require.True(t, cache.WaitForCacheSync(stop, ctrl.gatewaySynced))

	// Create a Gateway via the client (fake clientset requires Create, not pre-seeding).
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Port:     8080,
				Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
	_, err = client.GatewayV1().Gateways("default").Create(context.Background(), gw, metav1.CreateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	err = ctrl.updateGatewayStatus(gw)
	require.NoError(t, err)

	updated, err := client.GatewayV1().Gateways("default").Get(context.Background(), "test-gw", metav1.GetOptions{})
	require.NoError(t, err)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionTrue, programmed.Status)

	// Listener status should be populated.
	assert.Len(t, updated.Status.Listeners, 1)
	assert.Equal(t, gatewayv1.SectionName("http"), updated.Status.Listeners[0].Name)
	assert.NotEmpty(t, updated.Status.Listeners[0].SupportedKinds)
}

// getGateway returns the Gateway as the API server has it. The lister hands
// syncHandler the same object.
func getGateway(t *testing.T, client *gatewayfake.Clientset, name string) *gatewayv1.Gateway {
	t.Helper()
	gw, err := client.GatewayV1().Gateways("default").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return gw
}

// assertListenerConditions checks that each listener has the four conditions
// the controller owns, for the given generation.
func assertListenerConditions(t *testing.T, ls gatewayv1.ListenerStatus, generation int64) {
	t.Helper()
	for conditionType, status := range map[gatewayv1.ListenerConditionType]metav1.ConditionStatus{
		gatewayv1.ListenerConditionAccepted:     metav1.ConditionTrue,
		gatewayv1.ListenerConditionProgrammed:   metav1.ConditionTrue,
		gatewayv1.ListenerConditionResolvedRefs: metav1.ConditionTrue,
		gatewayv1.ListenerConditionConflicted:   metav1.ConditionFalse,
	} {
		condition := findCondition(ls.Conditions, string(conditionType))
		if assert.NotNil(t, condition, "listener %s condition %s", ls.Name, conditionType) {
			assert.Equal(t, status, condition.Status, "listener %s condition %s", ls.Name, conditionType)
			assert.Equal(t, generation, condition.ObservedGeneration, "listener %s condition %s", ls.Name, conditionType)
		}
	}
}

func TestUpdateGatewayStatus_FollowsSpecChanges(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)
	ctx := context.Background()

	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Generation = 1
	_, err := client.GatewayV1().Gateways("default").Create(ctx, gw, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))

	created := getGateway(t, client, "test-gw")
	acceptedSince := findCondition(created.Status.Conditions, string(gatewayv1.GatewayConditionAccepted)).LastTransitionTime
	listenerAcceptedSince := map[gatewayv1.SectionName]metav1.Time{
		"http": findCondition(created.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted)).LastTransitionTime,
	}

	steps := []struct {
		name      string
		listeners []gatewayv1.Listener
	}{
		{
			name:      "listener renamed and moved to another port",
			listeners: []gatewayv1.Listener{{Name: "web", Port: 9090, Protocol: gatewayv1.HTTPProtocolType}},
		},
		{
			name:      "listener moved to another port",
			listeners: []gatewayv1.Listener{{Name: "web", Port: 9091, Protocol: gatewayv1.HTTPProtocolType}},
		},
		{
			name: "listener added",
			listeners: []gatewayv1.Listener{
				{Name: "web", Port: 9091, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "admin", Port: 9092, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
		{
			name:      "listener removed",
			listeners: []gatewayv1.Listener{{Name: "admin", Port: 9092, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			generation := int64(i + 2)
			current := getGateway(t, client, "test-gw")
			current.Generation = generation
			current.Spec.Listeners = step.listeners
			stored, err := client.GatewayV1().Gateways("default").Update(ctx, current, metav1.UpdateOptions{})
			require.NoError(t, err)
			before := stored.DeepCopy()

			require.NoError(t, ctrl.updateGatewayStatus(stored))
			// The object handed in is the lister's cached copy and must not change.
			assert.Equal(t, before, stored)

			updated := getGateway(t, client, "test-gw")
			for _, conditionType := range []gatewayv1.GatewayConditionType{gatewayv1.GatewayConditionAccepted, gatewayv1.GatewayConditionProgrammed} {
				condition := findCondition(updated.Status.Conditions, string(conditionType))
				require.NotNil(t, condition)
				assert.Equal(t, metav1.ConditionTrue, condition.Status, conditionType)
				assert.Equal(t, generation, condition.ObservedGeneration, conditionType)
			}
			// The Gateway stays accepted, so the condition keeps its transition time.
			assert.Equal(t, acceptedSince, findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted)).LastTransitionTime)

			require.Len(t, updated.Status.Listeners, len(step.listeners))
			for j, listener := range step.listeners {
				ls := updated.Status.Listeners[j]
				assert.Equal(t, listener.Name, ls.Name)
				assertListenerConditions(t, ls, generation)
				accepted := findCondition(ls.Conditions, string(gatewayv1.ListenerConditionAccepted))
				require.NotNil(t, accepted)
				if since, ok := listenerAcceptedSince[ls.Name]; ok {
					assert.Equal(t, since, accepted.LastTransitionTime, "listener %s", ls.Name)
				}
				listenerAcceptedSince[ls.Name] = accepted.LastTransitionTime
			}
		})
	}
}

func TestUpdateGatewayStatus_RefreshesStaleStatus(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)
	ctx := context.Background()

	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Generation = 1
	_, err := client.GatewayV1().Gateways("default").Create(ctx, gw, metav1.CreateOptions{})
	require.NoError(t, err)

	// Accepted and Programmed are True for generation 1, without listener
	// status, and then the spec changes.
	since := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	current := getGateway(t, client, "test-gw")
	current.Status.Conditions = []metav1.Condition{
		{Type: string(gatewayv1.GatewayConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: since, Reason: string(gatewayv1.GatewayReasonAccepted)},
		{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: since, Reason: string(gatewayv1.GatewayReasonProgrammed)},
	}
	_, err = client.GatewayV1().Gateways("default").UpdateStatus(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)
	current = getGateway(t, client, "test-gw")
	current.Generation = 2
	current.Spec.Listeners[0].Port = 8081
	stored, err := client.GatewayV1().Gateways("default").Update(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, ctrl.updateGatewayStatus(stored))

	updated := getGateway(t, client, "test-gw")
	for _, conditionType := range []gatewayv1.GatewayConditionType{gatewayv1.GatewayConditionAccepted, gatewayv1.GatewayConditionProgrammed} {
		condition := findCondition(updated.Status.Conditions, string(conditionType))
		require.NotNil(t, condition)
		assert.Equal(t, int64(2), condition.ObservedGeneration, conditionType)
		assert.Equal(t, since, condition.LastTransitionTime, conditionType)
	}
	require.Len(t, updated.Status.Listeners, 1)
	assert.Equal(t, gatewayv1.SectionName("http"), updated.Status.Listeners[0].Name)
	assertListenerConditions(t, updated.Status.Listeners[0], 2)
}

func TestUpdateGatewayStatus_NoWriteWhenUpToDate(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)

	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Generation = 1
	_, err := client.GatewayV1().Gateways("default").Create(context.Background(), gw, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))
	require.Equal(t, 1, countStatusUpdates(client))

	require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))
	assert.Equal(t, 1, countStatusUpdates(client))
}

func TestUpdateGatewayStatus_KeepsConditionsOfNewerGeneration(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)

	// The Gateway is generation 1, but Accepted already observes generation 3:
	// this copy is older than the status, so Accepted is left alone.
	newer := metav1.Condition{
		Type: string(gatewayv1.GatewayConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 3,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), Reason: "Accepted", Message: "from generation 3",
	}
	newerListener := metav1.Condition{
		Type: string(gatewayv1.ListenerConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 3,
		LastTransitionTime: newer.LastTransitionTime, Reason: "Accepted", Message: "from generation 3",
	}
	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Generation = 1
	_, err := client.GatewayV1().Gateways("default").Create(context.Background(), gw, metav1.CreateOptions{})
	require.NoError(t, err)
	current := getGateway(t, client, "test-gw")
	current.Status.Conditions = []metav1.Condition{newer}
	current.Status.Listeners = []gatewayv1.ListenerStatus{{Name: "http", SupportedKinds: []gatewayv1.RouteGroupKind{}, Conditions: []metav1.Condition{newerListener}}}
	_, err = client.GatewayV1().Gateways("default").UpdateStatus(context.Background(), current, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))

	updated := getGateway(t, client, "test-gw")
	assert.Equal(t, newer, *findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted)))
	assert.Equal(t, newerListener, *findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted)))
	assert.Equal(t, int64(1), findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed)).ObservedGeneration)
}

func TestUpdateGatewayStatus_RepairsStatusAtSameGeneration(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)

	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Generation = 1
	_, err := client.GatewayV1().Gateways("default").Create(context.Background(), gw, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))

	// Someone removes a listener condition; the generation does not change.
	damaged := getGateway(t, client, "test-gw")
	damaged.Status.Listeners[0].Conditions = damaged.Status.Listeners[0].Conditions[:3]
	_, err = client.GatewayV1().Gateways("default").UpdateStatus(context.Background(), damaged, metav1.UpdateOptions{})
	require.NoError(t, err)
	stored := getGateway(t, client, "test-gw")
	before := stored.DeepCopy()

	require.NoError(t, ctrl.updateGatewayStatus(stored))

	assert.Equal(t, 2, countStatusUpdates(client)-1, "one repair after the first write and the damage")
	assertListenerConditions(t, getGateway(t, client, "test-gw").Status.Listeners[0], 1)
	// The object handed in is the lister's cached copy and must not change.
	assert.Equal(t, before, stored)
}

func TestUpdateGatewayStatus_GatewayClassOwner(t *testing.T) {
	tests := []struct {
		name       string
		class      *gatewayv1.GatewayClass
		wantWrites int
	}{
		{name: "GatewayClass of kthena-router", class: newTestGatewayClass(ControllerName), wantWrites: 1},
		{name: "GatewayClass not found", class: nil, wantWrites: 1},
		{name: "GatewayClass of another controller", class: newTestGatewayClass("example.net/edge"), wantWrites: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
			ctrl := newTestGatewayController(t, client)
			if tc.class != nil {
				_, err := client.GatewayV1().GatewayClasses().Create(context.Background(), tc.class, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			_, err := client.GatewayV1().Gateways("default").Create(context.Background(), newTestGateway("test-gw", "default", DefaultGatewayClassName), metav1.CreateOptions{})
			require.NoError(t, err)

			require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))
			assert.Equal(t, tc.wantWrites, countStatusUpdates(client))
		})
	}
}

func TestSyncHandler_GatewayClassHandedBack(t *testing.T) {
	ctx := context.Background()
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	classes := client.GatewayV1().GatewayClasses()
	_, err := classes.Create(ctx, newTestGatewayClass("example.net/edge"), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrl := startGatewayController(t, client, newTestGateway("test-gw", "default", DefaultGatewayClassName))
	ctrl.ownershipRecheckInterval = 10 * time.Millisecond
	go ctrl.runWorker()

	classGets := func() int {
		n := 0
		for _, action := range client.Actions() {
			if action.GetVerb() == "get" && action.GetResource().Resource == "gatewayclasses" {
				n++
			}
		}
		return n
	}
	// While another controller owns the class, the Gateway is checked again
	// but its status is left alone.
	require.Eventually(t, func() bool { return classGets() >= 3 }, 5*time.Second, 10*time.Millisecond)
	assert.Zero(t, countStatusUpdates(client))

	// spec.controllerName is immutable, so the class is handed back by
	// recreating it. No Gateway event follows.
	require.NoError(t, classes.Delete(ctx, DefaultGatewayClassName, metav1.DeleteOptions{}))
	_, err = classes.Create(ctx, newTestGatewayClass(ControllerName), metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		gw, err := client.GatewayV1().Gateways("default").Get(ctx, "test-gw", metav1.GetOptions{})
		if err != nil {
			return false
		}
		accepted := findCondition(gw.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
		return accepted != nil && accepted.Status == metav1.ConditionTrue
	}, 5*time.Second, 10*time.Millisecond, "the Gateway must be accepted after the class is handed back")
}

func TestUpdateGatewayStatus_ListenerWithUnknownProtocol(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)

	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Spec.Listeners = append(gw.Spec.Listeners, gatewayv1.Listener{Name: "bar", Port: 9000, Protocol: "example.com/bar"})
	_, err := client.GatewayV1().Gateways("default").Create(context.Background(), gw, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, ctrl.updateGatewayStatus(getGateway(t, client, "test-gw")))

	// supportedKinds is required: an empty list, not null, or the API server
	// rejects the whole status.
	updated := getGateway(t, client, "test-gw")
	require.Len(t, updated.Status.Listeners, 2)
	assert.NotNil(t, updated.Status.Listeners[1].SupportedKinds)
	assert.Empty(t, updated.Status.Listeners[1].SupportedKinds)
}

func TestUpdateGatewayStatus_UsesStoredResourceVersion(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := newTestGatewayController(t, client)

	// The API server has resourceVersion 2, with a renamed listener. The copy
	// that was stored is resourceVersion 1. Like the API server, the reactor
	// rejects a status update for an older resourceVersion.
	stored := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	stored.Generation, stored.ResourceVersion = 1, "1"
	latest := stored.DeepCopy()
	latest.Generation, latest.ResourceVersion = 2, "2"
	latest.Spec.Listeners[0].Name = "web"
	_, err := client.GatewayV1().Gateways("default").Create(context.Background(), latest, metav1.CreateOptions{})
	require.NoError(t, err)
	client.PrependReactor("update", "gateways", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateAction)
		resourceVersion := update.GetObject().(*gatewayv1.Gateway).ResourceVersion
		if update.GetSubresource() != "status" || resourceVersion == "" || resourceVersion == "2" {
			// Without a resourceVersion the API server applies the update.
			return false, nil, nil
		}
		return true, nil, apierrors.NewConflict(gatewayv1.Resource("gateways"), "test-gw", errors.New("the object has been modified"))
	})

	err = ctrl.updateGatewayStatus(stored)
	assert.True(t, apierrors.IsConflict(err), "got %v", err)
	assert.Empty(t, getGateway(t, client, "test-gw").Status.Listeners)
}

func TestSyncHandler_StatusUpdateErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr func(error) bool
	}{
		{
			name:    "conflict is not an error, the newer Gateway's event syncs it again",
			err:     apierrors.NewConflict(gatewayv1.Resource("gateways"), "test-gw", errors.New("the object has been modified")),
			wantErr: nil,
		},
		{
			name:    "server error is returned for a retry",
			err:     apierrors.NewInternalError(errors.New("etcd unavailable")),
			wantErr: apierrors.IsInternalError,
		},
		{
			name:    "Gateway deleted before the status update is not an error",
			err:     apierrors.NewNotFound(gatewayv1.Resource("gateways"), "test-gw"),
			wantErr: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
			ctrl := startGatewayController(t, client, newTestGateway("test-gw", "default", DefaultGatewayClassName))

			hits := 0
			client.PrependReactor("update", "gateways", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "status" {
					return false, nil, nil
				}
				hits++
				return true, nil, tc.err
			})

			err := ctrl.syncHandler("default/test-gw")
			assert.Equal(t, 1, hits)
			if tc.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.True(t, tc.wantErr(err), "got %v", err)
			}
		})
	}
}

func TestSyncHandler_StatusWriteEchoDoesNotWriteAgain(t *testing.T) {
	gw := newTestGateway("test-gw", "default", DefaultGatewayClassName)
	gw.Generation = 1
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	ctrl := startGatewayController(t, client, gw)

	// listerCaughtUp waits until the lister has the Gateway as the API server
	// has it: the update event of the last write has been delivered.
	// sync runs syncHandler and checks that it built the status from the
	// lister copy, without reading the Gateway from the API server.
	sync := func() {
		t.Helper()
		before := len(client.Actions())
		require.NoError(t, ctrl.syncHandler("default/test-gw"))
		for _, action := range client.Actions()[before:] {
			assert.False(t, action.GetVerb() == "get" && action.GetResource().Resource == "gateways", "unexpected Gateway GET")
		}
	}

	listerCaughtUp := func() {
		t.Helper()
		want := getGateway(t, client, "test-gw")
		require.Eventually(t, func() bool {
			listed, err := ctrl.gatewayLister.Gateways("default").Get("test-gw")
			return err == nil && listed.Generation == want.Generation && apiequality.Semantic.DeepEqual(listed.Status, want.Status)
		}, 5*time.Second, 10*time.Millisecond)
	}

	sync()
	require.Equal(t, 1, countStatusUpdates(client))
	// The write causes an update event, which syncs the Gateway again. That
	// sync finds the status up to date.
	listerCaughtUp()
	sync()
	assert.Equal(t, 1, countStatusUpdates(client))

	current := getGateway(t, client, "test-gw")
	current.Generation = 2
	current.Spec.Listeners[0].Port = 8081
	_, err := client.GatewayV1().Gateways("default").Update(context.Background(), current, metav1.UpdateOptions{})
	require.NoError(t, err)
	listerCaughtUp()
	sync()
	assert.Equal(t, 2, countStatusUpdates(client))
	listerCaughtUp()
	sync()
	assert.Equal(t, 2, countStatusUpdates(client))
}

// newTestGatewayController returns a controller for tests that call its
// methods directly, without informers.
func newTestGatewayController(t *testing.T, client *gatewayfake.Clientset) *GatewayController {
	t.Helper()
	ctrl, err := NewGatewayController(gatewayinformers.NewSharedInformerFactory(client, 0), datastore.New(), client)
	require.NoError(t, err)
	t.Cleanup(ctrl.workqueue.ShutDown)
	return ctrl
}

func newTestGatewayClass(controllerName string) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultGatewayClassName},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(controllerName)},
	}
}

// startGatewayController creates gw through the client, starts the informers
// and waits until the lister has it and the watch delivers updates. The fake
// client's watch does not replay objects created before it started, so the
// object is created first, and a label change confirms the watch is running.
func startGatewayController(t *testing.T, client *gatewayfake.Clientset, gw *gatewayv1.Gateway) *GatewayController {
	t.Helper()
	ctx := context.Background()
	_, err := client.GatewayV1().Gateways(gw.Namespace).Create(ctx, gw, metav1.CreateOptions{})
	require.NoError(t, err)
	informerFactory := gatewayinformers.NewSharedInformerFactory(client, 0)
	ctrl, err := NewGatewayController(informerFactory, datastore.New(), client)
	require.NoError(t, err)
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		ctrl.workqueue.ShutDown()
	})
	informerFactory.Start(stop)
	require.True(t, cache.WaitForCacheSync(stop, ctrl.gatewaySynced))

	// The cache can sync before the watch starts, and the fake client does not
	// replay an update made in between. Update a label until the lister has it.
	require.Eventually(t, func() bool {
		listed, err := ctrl.gatewayLister.Gateways(gw.Namespace).Get(gw.Name)
		if err == nil && listed.Labels["test/watch-ready"] == "true" {
			return true
		}
		marked, err := client.GatewayV1().Gateways(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		marked.Labels = map[string]string{"test/watch-ready": "true"}
		_, _ = client.GatewayV1().Gateways(gw.Namespace).Update(ctx, marked, metav1.UpdateOptions{})
		return false
	}, 5*time.Second, 50*time.Millisecond, "the watch must deliver updates before the test syncs")
	client.ClearActions()
	return ctrl
}

// countStatusUpdates returns how many Gateway status updates and patches the
// client received.
func countStatusUpdates(client *gatewayfake.Clientset) int {
	n := 0
	for _, action := range client.Actions() {
		if (action.GetVerb() == "update" || action.GetVerb() == "patch") && action.GetResource().Resource == "gateways" && action.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

func TestSyncHandler_RejectsForeignGatewayClass(t *testing.T) {
	client := gatewayfake.NewSimpleClientset() //nolint:staticcheck
	informerFactory := gatewayinformers.NewSharedInformerFactory(client, 0)
	store := datastore.New()
	ctrl, err := NewGatewayController(informerFactory, store, client)
	require.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	require.True(t, cache.WaitForCacheSync(stop, ctrl.gatewaySynced))

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "migrated-gw", Namespace: "default", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "istio",
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Port:     80,
				Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
	_, err = client.GatewayV1().Gateways("default").Create(context.Background(), gw, metav1.CreateOptions{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	err = ctrl.syncHandler("default/migrated-gw")
	require.NoError(t, err)

	// Gateway should NOT be in the store.
	assert.Nil(t, store.GetGateway("default/migrated-gw"))

	// Gateway status should NOT have been updated.
	updated, err := client.GatewayV1().Gateways("default").Get(context.Background(), "migrated-gw", metav1.GetOptions{})
	require.NoError(t, err)
	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	assert.Nil(t, accepted)
}

func TestBuildListenerStatuses(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "https", Port: 8443, Protocol: gatewayv1.HTTPSProtocolType},
			},
		},
	}
	statuses := buildListenerStatuses(gw)
	assert.Len(t, statuses, 2)
	assert.Equal(t, gatewayv1.SectionName("http"), statuses[0].Name)
	assert.Equal(t, gatewayv1.SectionName("https"), statuses[1].Name)

	// HTTP listener should support HTTPRoute.
	assert.Contains(t, statuses[0].SupportedKinds, gatewayv1.RouteGroupKind{
		Group: groupNamePtr(gatewayv1.GroupName), Kind: "HTTPRoute",
	})

	// HTTPS listener should support both HTTPRoute and GRPCRoute.
	assert.Len(t, statuses[1].SupportedKinds, 2)

	// Each listener should have Accepted, Programmed, ResolvedRefs, Conflicted.
	for _, ls := range statuses {
		assert.NotNil(t, findCondition(ls.Conditions, string(gatewayv1.ListenerConditionAccepted)))
		assert.NotNil(t, findCondition(ls.Conditions, string(gatewayv1.ListenerConditionProgrammed)))
		assert.NotNil(t, findCondition(ls.Conditions, string(gatewayv1.ListenerConditionResolvedRefs)))
		assert.NotNil(t, findCondition(ls.Conditions, string(gatewayv1.ListenerConditionConflicted)))
	}
}

func TestSupportedKindsForListener(t *testing.T) {
	tests := []struct {
		protocol gatewayv1.ProtocolType
		wantKind string
	}{
		{gatewayv1.HTTPProtocolType, "HTTPRoute"},
		{gatewayv1.HTTPSProtocolType, "HTTPRoute"},
		{gatewayv1.TLSProtocolType, "TLSRoute"},
		{gatewayv1.TCPProtocolType, "TCPRoute"},
		{gatewayv1.UDPProtocolType, "UDPRoute"},
	}
	for _, tc := range tests {
		t.Run(string(tc.protocol), func(t *testing.T) {
			kinds := supportedKindsForListener(gatewayv1.Listener{Protocol: tc.protocol})
			assert.NotEmpty(t, kinds)
			assert.Equal(t, tc.wantKind, string(kinds[0].Kind))
		})
	}
}

// findCondition is a test helper that finds a condition by type.
func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
