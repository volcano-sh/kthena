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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	ctrl, err := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	require.NoError(t, err)
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
	_, err = gatewayClient.GatewayV1().Gateways(ns).Create(ctx, gw, metav1.CreateOptions{})
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
	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	ctrl, err := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	require.NoError(t, err)
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
	_, err = gatewayClient.GatewayV1().Gateways(ns).Create(ctx, gw, metav1.CreateOptions{})
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

func TestHTTPRouteController_AllowedRoutesNamespaces(t *testing.T) {
	nsFromAll := gatewayv1.NamespacesFromAll
	nsFromSame := gatewayv1.NamespacesFromSame
	nsFromSelector := gatewayv1.NamespacesFromSelector

	tests := []struct {
		name          string
		routeNS       string
		routeLabels   map[string]string
		allowedRoutes *gatewayv1.AllowedRoutes
		defaultParent bool
		wantStored    bool
	}{
		{
			name:          "default same namespace allows same namespace",
			routeNS:       "default",
			defaultParent: true,
			wantStored:    true,
		},
		{
			name:       "default same namespace blocks cross namespace",
			routeNS:    "other",
			wantStored: false,
		},
		{
			name:    "all allows cross namespace",
			routeNS: "other",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &nsFromAll},
			},
			wantStored: true,
		},
		{
			name:        "selector allows matching namespace",
			routeNS:     "selected",
			routeLabels: map[string]string{"team": "inference"},
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From:     &nsFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "inference"}},
				},
			},
			wantStored: true,
		},
		{
			name:        "selector blocks non matching namespace",
			routeNS:     "blocked",
			routeLabels: map[string]string{"team": "other"},
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From:     &nsFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "inference"}},
				},
			},
			wantStored: false,
		},
		{
			name:    "explicit same namespace blocks cross namespace",
			routeNS: "other",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &nsFromSame},
			},
			wantStored: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespaces := []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			}
			if tt.routeNS != "default" {
				namespaces = append(namespaces, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tt.routeNS, Labels: tt.routeLabels}})
			}
			kubeClient := kubefake.NewSimpleClientset(namespaces...)
			kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
			gatewayClient := gatewayfake.NewSimpleClientset()
			gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
			store := datastore.New()

			gw := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gateway"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
					Listeners: []gatewayv1.Listener{
						{
							Name:          gatewayv1.SectionName("http"),
							Port:          gatewayv1.PortNumber(80),
							Protocol:      gatewayv1.HTTPProtocolType,
							AllowedRoutes: tt.allowedRoutes,
						},
					},
				},
			}
			assert.NoError(t, store.AddOrUpdateGateway(gw))

			parentRef := gatewayv1.ParentReference{
				Namespace: ptr(gatewayv1.Namespace("default")),
				Name:      gatewayv1.ObjectName("gateway"),
			}
			if !tt.defaultParent {
				parentRef.Kind = ptr(gatewayv1.Kind("Gateway"))
			}
			httpRoute := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: tt.routeNS, Name: "route"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{parentRef},
					},
				},
			}
			_, err := gatewayClient.GatewayV1().HTTPRoutes(tt.routeNS).Create(context.Background(), httpRoute, metav1.CreateOptions{})
			assert.NoError(t, err)

			ctrl, err := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
			require.NoError(t, err)
			stop := make(chan struct{})
			defer close(stop)
			kubeInformerFactory.Start(stop)
			gatewayInformerFactory.Start(stop)

			if !cache.WaitForCacheSync(stop,
				kubeInformerFactory.Core().V1().Namespaces().Informer().HasSynced,
				gatewayInformerFactory.Gateway().V1().HTTPRoutes().Informer().HasSynced,
			) {
				t.Fatal("cache sync timeout")
			}

			err = ctrl.syncHandler(tt.routeNS + "/route")
			assert.NoError(t, err)
			assert.Equal(t, tt.wantStored, store.GetHTTPRoute(tt.routeNS+"/route") != nil)
			assert.Equal(t, tt.wantStored, len(store.GetHTTPRoutesByGateway("default/gateway")) > 0)
		})
	}
}

// TestHTTPRouteController_MultipleParentRefs_FirstPending verifies that when the first parentRef
// references a Gateway not in store, we still process if a later parentRef matches.
func TestHTTPRouteController_MultipleParentRefs_FirstPending(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	ctx := context.Background()
	ns := "default"
	gw2 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-2"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     gatewayv1.SectionName("http"),
					Port:     gatewayv1.PortNumber(80),
					Protocol: gatewayv1.HTTPProtocolType,
				},
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

	ctrl, err := NewHTTPRouteController(gatewayInformerFactory, kubeInformerFactory, store)
	require.NoError(t, err)
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
