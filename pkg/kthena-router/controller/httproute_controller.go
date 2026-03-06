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
	"fmt"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type HTTPRouteController struct {
	httpRouteLister gatewaylisters.HTTPRouteLister
	httpRouteSynced cache.InformerSynced
	registration    cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewHTTPRouteController(
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	store datastore.Store,
) *HTTPRouteController {
	httpRouteInformer := gatewayInformerFactory.Gateway().V1().HTTPRoutes()

	controller := &HTTPRouteController{
		httpRouteLister: httpRouteInformer.Lister(),
		httpRouteSynced: httpRouteInformer.Informer().HasSynced,
		workqueue:       workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:     &atomic.Bool{},
		store:           store,
	}

	controller.registration, _ = httpRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueHTTPRoute,
		UpdateFunc: func(old, new interface{}) { controller.enqueueHTTPRoute(new) },
		DeleteFunc: controller.enqueueHTTPRoute,
	})

	return controller
}

func (c *HTTPRouteController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.httpRouteSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *HTTPRouteController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *HTTPRouteController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *HTTPRouteController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial http routes have been synced")
		c.workqueue.Forget(obj)
		c.initialSync.Store(true)
		return true
	}

	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		c.workqueue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	if err := c.syncHandler(key); err != nil {
		if c.workqueue.NumRequeues(key) < maxRetries {
			klog.Errorf("error syncing httproute %q: %s, requeuing", key, err.Error())
			c.workqueue.AddRateLimited(key)
			return true
		}
		klog.Errorf("giving up on syncing httproute %q after %d retries: %s", key, maxRetries, err)
		c.workqueue.Forget(obj)
	}
	return true
}

func (c *HTTPRouteController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	httpRoute, err := c.httpRouteLister.HTTPRoutes(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		_ = c.store.DeleteModelRoute(key)
		_ = c.store.DeleteHTTPRoute(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Only process HTTPRoutes that reference kthena-router GatewayClass
	// Check if any parentRef references a Gateway with kthena-router GatewayClass
	shouldProcess := false
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
			gatewayNamespace := httpRoute.Namespace
			if parentRef.Namespace != nil {
				gatewayNamespace = string(*parentRef.Namespace)
			}
			gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, string(parentRef.Name))
			gw := c.store.GetGateway(gatewayKey)
			if gw != nil && gw.Spec.GatewayClassName != "" {
				if string(gw.Spec.GatewayClassName) == DefaultGatewayClassName {
					shouldProcess = true
					break
				}
			}
		}
	}

	if !shouldProcess {
		klog.V(4).Infof("Skipping HTTPRoute %s/%s: does not reference kthena-router Gateway", namespace, name)
		return nil
	}

	// Translate HTTPRoute to ModelRoute
	translatedMR := &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      httpRoute.Name,
			Namespace: httpRoute.Namespace,
			Labels: map[string]string{
				"kthena.serving.volcano.sh/generated-from": "HTTPRoute",
			},
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			ParentRefs: httpRoute.Spec.ParentRefs,
			Rules:      make([]*aiv1alpha1.Rule, 0, len(httpRoute.Spec.Rules)),
		},
	}

	for _, hrRule := range httpRoute.Spec.Rules {
		targetModels := make([]*aiv1alpha1.TargetModel, 0, len(hrRule.BackendRefs))
		// Map BackendRefs to TargetModels
		for _, backendRef := range hrRule.BackendRefs {
			if backendRef.Kind != nil && *backendRef.Kind == "InferencePool" {
				weight := uint32(100)
				if backendRef.Weight != nil {
					weight = uint32(*backendRef.Weight)
				}
				targetModels = append(targetModels, &aiv1alpha1.TargetModel{
					ModelServerName: string(backendRef.Name),
					Weight:          &weight,
				})
			}
		}

		if len(targetModels) == 0 {
			continue
		}

		// Map Matches to ModelMatch
		// If no matches are specified, it matches everything (empty ModelMatch)
		if len(hrRule.Matches) == 0 {
			rule := &aiv1alpha1.Rule{
				TargetModels: targetModels,
			}
			translatedMR.Spec.Rules = append(translatedMR.Spec.Rules, rule)
			continue
		}

		// Create a separate rule for each match
		for _, match := range hrRule.Matches {
			rule := &aiv1alpha1.Rule{
				TargetModels: targetModels,
				ModelMatch: &aiv1alpha1.ModelMatch{
					Headers: make(map[string]*aiv1alpha1.StringMatch),
				},
			}

			if match.Path != nil && match.Path.Value != nil {
				rule.ModelMatch.Uri = &aiv1alpha1.StringMatch{}
				val := *match.Path.Value
				if match.Path.Type != nil {
					switch *match.Path.Type {
					case gatewayv1.PathMatchExact:
						rule.ModelMatch.Uri.Exact = &val
					case gatewayv1.PathMatchPathPrefix:
						rule.ModelMatch.Uri.Prefix = &val
					case gatewayv1.PathMatchRegularExpression:
						rule.ModelMatch.Uri.Regex = &val
					}
				}
			}
			for _, header := range match.Headers {
				headerVal := string(header.Value)
				sm := &aiv1alpha1.StringMatch{}
				if header.Type == nil || *header.Type == gatewayv1.HeaderMatchExact {
					sm.Exact = &headerVal
				} else if *header.Type == gatewayv1.HeaderMatchRegularExpression {
					sm.Regex = &headerVal
				}
				rule.ModelMatch.Headers[string(header.Name)] = sm
			}
			translatedMR.Spec.Rules = append(translatedMR.Spec.Rules, rule)
		}
	}

	if err := c.store.AddOrUpdateModelRoute(translatedMR); err != nil {
		return err
	}

	return c.store.AddOrUpdateHTTPRoute(httpRoute)
}

func (c *HTTPRouteController) enqueueHTTPRoute(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
