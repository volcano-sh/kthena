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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func ptr[T any](v T) *T { return &v }

func TestHTTPRouteController_EnqueueHTTPRoutesForGateway(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	ctrl := NewHTTPRouteController(gatewayInformerFactory, store)
	ns := "default"
	emptyGroup := gatewayv1.Group("")
	emptyKind := gatewayv1.Kind("")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
		},
	}

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-1"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName("gateway-1")},
				},
			},
		},
	}

	httpRoute2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-2"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("other-gateway")},
				},
			},
		},
	}

	httpRoute3 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "route-3"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("gateway-1"), Namespace: ptr(gatewayv1.Namespace(ns))},
				},
			},
		},
	}

	httpRoute4 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-4"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Group: &emptyGroup, Kind: &emptyKind, Name: gatewayv1.ObjectName("gateway-1")},
				},
			},
		},
	}

	addHTTPRoutesToInformer(t, gatewayInformerFactory, httpRoute, httpRoute2, httpRoute3, httpRoute4)
	ctrl.enqueueHTTPRoutesForGateway(gw)

	assert.ElementsMatch(t, []string{
		"default/route-1",
		"other-ns/route-3",
		"default/route-4",
	}, drainHTTPRouteQueue(ctrl), "route-1, route-3, and route-4 reference gateway-1, route-2 does not")
}

func TestHTTPRouteController_EnqueueHTTPRoutesForGateway_NoMatchingRoutes(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	ctrl := NewHTTPRouteController(gatewayInformerFactory, store)
	ns := "default"
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
		},
	}

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-1"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("other-gateway")},
				},
			},
		},
	}

	serviceGroup := gatewayv1.Group("core")
	serviceKind := gatewayv1.Kind("Service")
	httpRoute2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-2"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Group: &serviceGroup, Kind: &serviceKind, Name: gatewayv1.ObjectName("gateway-1")},
				},
			},
		},
	}

	addHTTPRoutesToInformer(t, gatewayInformerFactory, httpRoute, httpRoute2)
	ctrl.enqueueHTTPRoutesForGateway(gw)
	assert.Empty(t, drainHTTPRouteQueue(ctrl), "no HTTPRoutes reference gateway-1 as a Gateway parent")
}

// TestHTTPRouteController_MultipleParentRefs_FirstPending verifies that when the first parentRef
// references a Gateway not in store, we still process if a later parentRef matches.
func TestHTTPRouteController_MultipleParentRefs_FirstPending(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	ctx := context.Background()
	ns := "default"
	gw2 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-2"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
		},
	}
	_, err := gatewayClient.GatewayV1().Gateways(ns).Create(ctx, gw2, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, store.AddOrUpdateGateway(gw2))

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-multi"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("gateway-1")},
					{Name: gatewayv1.ObjectName("gateway-2")},
				},
			},
		},
	}
	_, err = gatewayClient.GatewayV1().HTTPRoutes(ns).Create(ctx, httpRoute, metav1.CreateOptions{})
	assert.NoError(t, err)

	ctrl := NewHTTPRouteController(gatewayInformerFactory, store)
	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	if !cache.WaitForCacheSync(stop, gatewayInformerFactory.Gateway().V1().HTTPRoutes().Informer().HasSynced) {
		t.Fatal("cache sync timeout")
	}

	err = ctrl.syncHandler(ns + "/route-multi")
	assert.NoError(t, err)
	assert.NotNil(t, store.GetHTTPRoute(ns+"/route-multi"))
}

func addHTTPRoutesToInformer(t *testing.T, factory gatewayinformers.SharedInformerFactory, routes ...*gatewayv1.HTTPRoute) {
	t.Helper()

	store := factory.Gateway().V1().HTTPRoutes().Informer().GetStore()
	for _, route := range routes {
		require.NoError(t, store.Add(route))
	}
}

func drainHTTPRouteQueue(ctrl *HTTPRouteController) []string {
	keys := []string{}
	for ctrl.workqueue.Len() > 0 {
		obj, _ := ctrl.workqueue.Get()
		if key, ok := obj.(string); ok {
			keys = append(keys, key)
		}
		ctrl.workqueue.Done(obj)
		ctrl.workqueue.Forget(obj)
	}
	return keys
}
