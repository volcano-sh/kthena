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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
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
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubefake.NewSimpleClientset(), 0)
	store := datastore.New()

	ctrl := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	ctx := context.Background()
	ns := "default"
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
		},
	}
	_, err := gatewayClient.GatewayV1().Gateways(ns).Create(ctx, gw, metav1.CreateOptions{})
	assert.NoError(t, err)

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-1"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("gateway-1")},
				},
			},
		},
	}
	_, err = gatewayClient.GatewayV1().HTTPRoutes(ns).Create(ctx, httpRoute, metav1.CreateOptions{})
	assert.NoError(t, err)

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
	_, err = gatewayClient.GatewayV1().HTTPRoutes(ns).Create(ctx, httpRoute2, metav1.CreateOptions{})
	assert.NoError(t, err)

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
	_, err = gatewayClient.GatewayV1().HTTPRoutes("other-ns").Create(ctx, httpRoute3, metav1.CreateOptions{})
	assert.NoError(t, err)

	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()
	if !cache.WaitForCacheSync(stop, gatewayInformer.Informer().HasSynced) {
		t.Fatal("gateway cache sync timeout")
	}
	httpRouteInformer := gatewayInformerFactory.Gateway().V1().HTTPRoutes()
	if !cache.WaitForCacheSync(stop, httpRouteInformer.Informer().HasSynced) {
		t.Fatal("httproute cache sync timeout")
	}

	time.Sleep(200 * time.Millisecond)
	for ctrl.workqueue.Len() > 0 {
		obj, _ := ctrl.workqueue.Get()
		ctrl.workqueue.Done(obj)
	}

	ctrl.enqueueHTTPRoutesForGateway(gw)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 2, ctrl.workqueue.Len(), "route-1 and route-3 reference gateway-1, route-2 does not")
}

func TestHTTPRouteController_EnqueueHTTPRoutesForGateway_NoMatchingRoutes(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubefake.NewSimpleClientset(), 0)
	store := datastore.New()

	ctrl := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	ctx := context.Background()
	ns := "default"
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
		},
	}
	_, err := gatewayClient.GatewayV1().Gateways(ns).Create(ctx, gw, metav1.CreateOptions{})
	assert.NoError(t, err)

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
	_, err = gatewayClient.GatewayV1().HTTPRoutes(ns).Create(ctx, httpRoute, metav1.CreateOptions{})
	assert.NoError(t, err)

	if !cache.WaitForCacheSync(stop, gatewayInformerFactory.Gateway().V1().HTTPRoutes().Informer().HasSynced) {
		t.Fatal("cache sync timeout")
	}

	time.Sleep(200 * time.Millisecond)
	for ctrl.workqueue.Len() > 0 {
		obj, _ := ctrl.workqueue.Get()
		ctrl.workqueue.Done(obj)
	}

	ctrl.enqueueHTTPRoutesForGateway(gw)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, ctrl.workqueue.Len(), "no HTTPRoutes reference gateway-1")
}

// TestHTTPRouteController_MultipleParentRefs_FirstPending verifies that when the first parentRef
// references a Gateway not in store, we still process if a later parentRef matches.
func TestHTTPRouteController_MultipleParentRefs_FirstPending(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubefake.NewSimpleClientset(), 0)
	store := datastore.New()

	ctx := context.Background()
	ns := "default"
	gw2 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-2"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
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
					{Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("gateway-2")},
				},
			},
		},
	}
	_, err = gatewayClient.GatewayV1().HTTPRoutes(ns).Create(ctx, httpRoute, metav1.CreateOptions{})
	assert.NoError(t, err)

	ctrl := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
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

func TestHTTPRouteController_AllowedRoutesDefaultSameNamespace(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	ctx := context.Background()
	gatewayNamespace := "default"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{
				{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	_, err := gatewayClient.GatewayV1().Gateways(gatewayNamespace).Create(ctx, gateway, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, store.AddOrUpdateGateway(gateway))

	sameNamespaceRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "same-route"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName("gateway-1")}},
			},
		},
	}
	_, err = gatewayClient.GatewayV1().HTTPRoutes(gatewayNamespace).Create(ctx, sameNamespaceRoute, metav1.CreateOptions{})
	assert.NoError(t, err)

	otherNamespaceRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "other-route"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName("gateway-1"), Namespace: ptr(gatewayv1.Namespace(gatewayNamespace))}},
			},
		},
	}
	_, err = gatewayClient.GatewayV1().HTTPRoutes("other-ns").Create(ctx, otherNamespaceRoute, metav1.CreateOptions{})
	assert.NoError(t, err)

	ctrl := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	if !cache.WaitForCacheSync(stop,
		gatewayInformerFactory.Gateway().V1().HTTPRoutes().Informer().HasSynced,
		kubeInformerFactory.Core().V1().Namespaces().Informer().HasSynced,
	) {
		t.Fatal("cache sync timeout")
	}

	assert.NoError(t, ctrl.syncHandler(gatewayNamespace+"/same-route"))
	assert.NotNil(t, store.GetHTTPRoute(gatewayNamespace+"/same-route"))

	assert.NoError(t, ctrl.syncHandler("other-ns/other-route"))
	assert.Nil(t, store.GetHTTPRoute("other-ns/other-route"))
}

func TestHTTPRouteController_AllowedRoutesAllNamespaces(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	ctx := context.Background()
	fromAll := gatewayv1.NamespacesFromAll
	gatewayNamespace := "default"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
					},
				},
			},
		},
	}
	_, err := gatewayClient.GatewayV1().Gateways(gatewayNamespace).Create(ctx, gateway, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, store.AddOrUpdateGateway(gateway))

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "route"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName("gateway-1"), Namespace: ptr(gatewayv1.Namespace(gatewayNamespace))}},
			},
		},
	}
	_, err = gatewayClient.GatewayV1().HTTPRoutes("other-ns").Create(ctx, route, metav1.CreateOptions{})
	assert.NoError(t, err)

	ctrl := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	if !cache.WaitForCacheSync(stop, gatewayInformerFactory.Gateway().V1().HTTPRoutes().Informer().HasSynced) {
		t.Fatal("cache sync timeout")
	}

	assert.NoError(t, ctrl.syncHandler("other-ns/route"))
	assert.NotNil(t, store.GetHTTPRoute("other-ns/route"))
}

func TestHTTPRouteController_AllowedRoutesNamespaceSelector(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "selected-ns", Labels: map[string]string{"team": "ai"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "blocked-ns", Labels: map[string]string{"team": "ops"}}},
	)
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	ctx := context.Background()
	fromSelector := gatewayv1.NamespacesFromSelector
	gatewayNamespace := "default"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "gateway-1"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From:     &fromSelector,
							Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "ai"}},
						},
					},
				},
			},
		},
	}
	_, err := gatewayClient.GatewayV1().Gateways(gatewayNamespace).Create(ctx, gateway, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, store.AddOrUpdateGateway(gateway))

	for _, namespace := range []string{"selected-ns", "blocked-ns"} {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "route"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName("gateway-1"), Namespace: ptr(gatewayv1.Namespace(gatewayNamespace))}},
				},
			},
		}
		_, err = gatewayClient.GatewayV1().HTTPRoutes(namespace).Create(ctx, route, metav1.CreateOptions{})
		assert.NoError(t, err)
	}

	ctrl := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	if !cache.WaitForCacheSync(stop,
		gatewayInformerFactory.Gateway().V1().HTTPRoutes().Informer().HasSynced,
		kubeInformerFactory.Core().V1().Namespaces().Informer().HasSynced,
	) {
		t.Fatal("cache sync timeout")
	}

	assert.NoError(t, ctrl.syncHandler("selected-ns/route"))
	assert.NotNil(t, store.GetHTTPRoute("selected-ns/route"))

	assert.NoError(t, ctrl.syncHandler("blocked-ns/route"))
	assert.Nil(t, store.GetHTTPRoute("blocked-ns/route"))
}
