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

func TestUpdateGatewayStatus_SkipWhenAlreadySet(t *testing.T) {
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

	// Pre-set status to Accepted=True and Programmed=True.
	gw.Status = gatewayv1.GatewayStatus{
		Conditions: []metav1.Condition{
			{Type: string(gatewayv1.GatewayConditionAccepted), Status: metav1.ConditionTrue},
			{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue},
		},
	}
	_, err = client.GatewayV1().Gateways("default").UpdateStatus(context.Background(), gw, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = ctrl.updateGatewayStatus(gw)
	require.NoError(t, err)

	// Status should remain unchanged — skipped because conditions are already True.
	updated, err := client.GatewayV1().Gateways("default").Get(context.Background(), "test-gw", metav1.GetOptions{})
	require.NoError(t, err)
	// Listeners should not be populated because we skipped the write.
	assert.Empty(t, updated.Status.Listeners)
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
