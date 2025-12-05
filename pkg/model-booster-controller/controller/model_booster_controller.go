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
	"fmt"
	"net/http"
	"strings"
	"time"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	networkingLister "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	workloadLister "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/config"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
)

const (
	ConfigMapName = "model-booster-controller-config"
)

type ModelBoosterController struct {
	// Client for k8s. Use it to call K8S API
	kubeClient kubernetes.Interface
	// client for custom resource
	client clientset.Interface
	// httpClient for HTTP requests to LoRA adapter APIs
	httpClient *http.Client

	syncHandler                       func(ctx context.Context, miKey string) error
	modelBoosterLister                workloadLister.ModelBoosterLister
	modelsInformer                    cache.Controller
	modelServingLister                workloadLister.ModelServingLister
	modelServingInformer              cache.SharedIndexInformer
	modelServersLister                networkingLister.ModelServerLister
	modelServersInformer              cache.SharedIndexInformer
	modelRoutesLister                 networkingLister.ModelRouteLister
	modelRoutesInformer               cache.SharedIndexInformer
	autoscalingPoliciesLister         workloadLister.AutoscalingPolicyLister
	autoscalingPoliciesInformer       cache.SharedIndexInformer
	autoscalingPolicyBindingsLister   workloadLister.AutoscalingPolicyBindingLister
	autoscalingPolicyBindingsInformer cache.SharedIndexInformer
	podsLister                        listerv1.PodLister
	podsInformer                      cache.SharedIndexInformer
	kubeInformerFactory               informers.SharedInformerFactory
	workQueue                         workqueue.TypedRateLimitingInterface[any]
	// loraUpdateCache stores the previous model version for LoRA adapter comparison
	// Key format: "namespace/name:generation" to avoid version conflicts
	loraUpdateCache map[string]*workload.ModelBooster
}

func (mc *ModelBoosterController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer mc.workQueue.ShutDown()

	// start informers
	go mc.modelsInformer.RunWithContext(ctx)
	go mc.modelServingInformer.RunWithContext(ctx)
	go mc.autoscalingPoliciesInformer.RunWithContext(ctx)
	go mc.autoscalingPolicyBindingsInformer.RunWithContext(ctx)
	go mc.podsInformer.RunWithContext(ctx)
	go mc.modelServersInformer.RunWithContext(ctx)
	go mc.modelRoutesInformer.RunWithContext(ctx)

	// start Kubernetes informer factory
	go mc.kubeInformerFactory.Start(ctx.Done())

	cache.WaitForCacheSync(ctx.Done(),
		mc.modelsInformer.HasSynced,
		mc.modelServingInformer.HasSynced,
		mc.autoscalingPoliciesInformer.HasSynced,
		mc.autoscalingPolicyBindingsInformer.HasSynced,
		mc.podsInformer.HasSynced,
		mc.modelServersInformer.HasSynced,
		mc.modelRoutesInformer.HasSynced,
	)

	klog.Info("start model controller")
	for i := 0; i < workers; i++ {
		go mc.worker(ctx)
	}
	<-ctx.Done()
	klog.Info("shut down model controller")
}

func (mc *ModelBoosterController) worker(ctx context.Context) {
	for mc.processNextWorkItem(ctx) {
	}
}

func (mc *ModelBoosterController) processNextWorkItem(ctx context.Context) bool {
	key, quit := mc.workQueue.Get()
	if quit {
		return false
	}
	defer mc.workQueue.Done(key)

	err := mc.syncHandler(ctx, key.(string))
	if err == nil {
		mc.workQueue.Forget(key)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	mc.workQueue.AddRateLimited(key)
	return true
}

func (mc *ModelBoosterController) createModelBooster(obj any) {
	model, ok := obj.(*workload.ModelBooster)
	if !ok {
		klog.Error("failed to parse ModelBooster when createModelBooster")
		return
	}
	klog.V(4).Infof("Create model: %s", klog.KObj(model))
	mc.enqueueModelBooster(model)
}

func (mc *ModelBoosterController) enqueueModelBooster(model *workload.ModelBooster) {
	if key, err := cache.MetaNamespaceKeyFunc(model); err != nil {
		utilruntime.HandleError(err)
	} else {
		mc.workQueue.Add(key)
	}
}

func (mc *ModelBoosterController) updateModelBooster(old any, new any) {
	newModel, ok := new.(*workload.ModelBooster)
	if !ok {
		klog.Error("failed to parse new ModelBooster when updateModelBooster")
		return
	}
	oldModel, ok := old.(*workload.ModelBooster)
	if !ok {
		klog.Error("failed to parse old ModelBooster when updateModelBooster")
		return
	}

	// When observed generation not equal to generation, reconcile model
	if oldModel.Status.ObservedGeneration != newModel.Generation {
		// Store the old model in cache with generation-specific key to avoid conflicts
		cacheKey := fmt.Sprintf("%s/%s:%d", newModel.Namespace, newModel.Name, newModel.Generation)
		mc.loraUpdateCache[cacheKey] = oldModel.DeepCopy()

		mc.enqueueModelBooster(newModel)
	}
}

func (mc *ModelBoosterController) deleteModelBooster(obj any) {
	model, ok := obj.(*workload.ModelBooster)
	if !ok {
		klog.Error("failed to parse ModelBooster when deleteModelBooster")
		return
	}
	klog.V(4).Infof("Delete model: %s", klog.KObj(model))
}

// reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (mc *ModelBoosterController) reconcile(ctx context.Context, namespaceAndName string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(namespaceAndName)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", err)
	}
	model, err := mc.modelBoosterLister.ModelBoosters(namespace).Get(name)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	klog.InfoS("Start to process model", "namespace", namespace, "model name", model.Name, "model status", model.Status)
	if len(model.Status.Conditions) == 0 {
		if err := mc.setModelInitCondition(ctx, model); err != nil {
			return err
		}
	}
	if err := mc.setModelProcessingCondition(ctx, model); err != nil {
		return err
	}
	if err := mc.createOrUpdateModelServing(ctx, model); err != nil {
		mc.setModelFailedCondition(ctx, model, err)
		return err
	}
	if err := mc.createOrUpdateModelServer(ctx, model); err != nil {
		mc.setModelFailedCondition(ctx, model, err)
		return err
	}
	if err := mc.createOrUpdateModelRoute(ctx, model); err != nil {
		mc.setModelFailedCondition(ctx, model, err)
		return err
	}
	if err := mc.createOrUpdateAutoscalingPolicyAndBinding(ctx, model); err != nil {
		mc.setModelFailedCondition(ctx, model, err)
		return err
	}
	modelServingActive, err := mc.isModelServingActive(model)
	if err != nil || !modelServingActive {
		return err
	}
	if err := mc.setModelActiveCondition(ctx, model); err != nil {
		return err
	}

	return nil
}

// isModelServingActive returns true if all ModelServings are available.
func (mc *ModelBoosterController) isModelServingActive(model *workload.ModelBooster) (bool, error) {
	modelServings, err := mc.listModelServingsByLabel(model)
	if err != nil {
		return false, err
	}
	// Ensure exactly one ModelServing exists for the single backend
	if len(modelServings) != 1 {
		klog.Infof("Number of ModelServings: %d, expected: %d", len(modelServings), 1)
		return false, fmt.Errorf("ModelServing number not equal to backend number")
	}
	// Check if all ModelServings are available
	for _, modelServing := range modelServings {
		if !meta.IsStatusConditionPresentAndEqual(modelServing.Status.Conditions, string(workload.ModelServingAvailable), metav1.ConditionTrue) {
			// requeue until all ModelServings are active
			klog.InfoS("ModelServing is not available", "ModelServing", klog.KObj(modelServing))
			return false, nil
		}
	}
	return true, nil
}

// updateModelBoosterStatus updates model status.
func (mc *ModelBoosterController) updateModelBoosterStatus(ctx context.Context, modelBooster *workload.ModelBooster) error {
	modelBooster.Status.ObservedGeneration = modelBooster.Generation
	if _, err := mc.client.WorkloadV1alpha1().ModelBoosters(modelBooster.Namespace).UpdateStatus(ctx, modelBooster, metav1.UpdateOptions{}); err != nil {
		klog.Errorf("update modelBooster status failed: %v", err)
		return err
	}

	// Clean up outdated cache entries for this modelBooster
	mc.cleanupOutdatedLoraUpdateCache(modelBooster)
	return nil
}

// cleanupOutdatedLoraUpdateCache removes stale cache entries for the given modelBooster
// Cache key format: "namespace/name:generation"
func (mc *ModelBoosterController) cleanupOutdatedLoraUpdateCache(modelBooster *workload.ModelBooster) {
	prefix := fmt.Sprintf("%s/%s:", modelBooster.Namespace, modelBooster.Name)
	for key := range mc.loraUpdateCache {
		if strings.HasPrefix(key, prefix) {
			// Keep only the current generation entry, remove others
			expectedKey := fmt.Sprintf("%s%d", prefix, modelBooster.Generation)
			if key != expectedKey {
				delete(mc.loraUpdateCache, key)
			}
		}
	}
}

func NewModelBoosterController(kubeClient kubernetes.Interface, client clientset.Interface) *ModelBoosterController {
	selector, err := labels.NewRequirement(utils.ManageBy, selection.Equals, []string{workload.GroupName})
	if err != nil {
		klog.Errorf("cannot create label selector, err: %v", err)
		return nil
	}

	filterInformerFactory := informersv1alpha1.NewSharedInformerFactoryWithOptions(
		client,
		0,
		informersv1alpha1.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = selector.String()
		}),
	)

	informerFactory := informersv1alpha1.NewSharedInformerFactory(client, 0)
	modelBoosterInformer := informerFactory.Workload().V1alpha1().ModelBoosters()
	modelServingInformer := filterInformerFactory.Workload().V1alpha1().ModelServings()
	modelServerInformer := filterInformerFactory.Networking().V1alpha1().ModelServers()
	modelRouteInformer := filterInformerFactory.Networking().V1alpha1().ModelRoutes()
	autoscalingPoliciesInformer := filterInformerFactory.Workload().V1alpha1().AutoscalingPolicies()
	autoscalingPolicyBindingsInformer := filterInformerFactory.Workload().V1alpha1().AutoscalingPolicyBindings()

	// Initialize Kubernetes informer factory for pods
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podsInformer := kubeInformerFactory.Core().V1().Pods().Informer()
	podsLister := kubeInformerFactory.Core().V1().Pods().Lister()

	// Create a shared HTTP client for LoRA adapter API calls
	// This client will be reused across all HTTP requests, enabling connection pooling
	httpClient := &http.Client{
		Timeout: 5 * time.Minute, // Default timeout for LoRA adapter operations
		Transport: &http.Transport{
			MaxIdleConns:        100,              // Maximum number of idle connections
			MaxIdleConnsPerHost: 10,               // Maximum idle connections per host
			IdleConnTimeout:     90 * time.Second, // How long an idle connection is kept
		},
	}

	mc := &ModelBoosterController{
		kubeClient:                        kubeClient,
		client:                            client,
		httpClient:                        httpClient,
		modelBoosterLister:                modelBoosterInformer.Lister(),
		modelsInformer:                    modelBoosterInformer.Informer(),
		modelServingLister:                modelServingInformer.Lister(),
		modelServingInformer:              modelServingInformer.Informer(),
		modelServersLister:                modelServerInformer.Lister(),
		modelServersInformer:              modelServerInformer.Informer(),
		modelRoutesLister:                 modelRouteInformer.Lister(),
		modelRoutesInformer:               modelRouteInformer.Informer(),
		autoscalingPoliciesLister:         autoscalingPoliciesInformer.Lister(),
		autoscalingPoliciesInformer:       autoscalingPoliciesInformer.Informer(),
		autoscalingPolicyBindingsLister:   autoscalingPolicyBindingsInformer.Lister(),
		autoscalingPolicyBindingsInformer: autoscalingPolicyBindingsInformer.Informer(),
		podsLister:                        podsLister,
		podsInformer:                      podsInformer,
		kubeInformerFactory:               kubeInformerFactory,
		loraUpdateCache:                   make(map[string]*workload.ModelBooster),

		workQueue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[any](),
			workqueue.TypedRateLimitingQueueConfig[any]{}),
	}
	klog.Info("Set the ModelBooster event handler")
	_, err = modelBoosterInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    mc.createModelBooster,
		UpdateFunc: mc.updateModelBooster,
		DeleteFunc: mc.deleteModelBooster,
	})
	if err != nil {
		klog.Fatal("Unable to add model event handler")
		return nil
	}
	_, err = modelServingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: mc.triggerModel,
		DeleteFunc: mc.deleteModelServing,
	})
	if err != nil {
		klog.Fatal("Unable to add ModelServing event handler")
		return nil
	}
	_, err = modelRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: mc.deleteModelRoute,
	})
	if err != nil {
		klog.Fatal("Unable to add model route event handler")
		return nil
	}
	_, err = modelServerInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: mc.deleteModelServer,
	})
	if err != nil {
		klog.Fatal("Unable to add model server event handler")
		return nil
	}
	mc.syncHandler = mc.reconcile
	mc.loadConfigFromConfigMap()
	return mc
}

func (mc *ModelBoosterController) loadConfigFromConfigMap() {
	namespace, err := utils.GetInClusterNameSpace()
	// When run locally, namespace will be empty, default value of downloader image and runtime image will be used.
	// So we don't need to read ConfigMap in this case.
	if len(namespace) == 0 {
		klog.Warning(err)
		return
	}
	cm, err := mc.kubeClient.CoreV1().ConfigMaps(namespace).Get(context.Background(), ConfigMapName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("ConfigMap does not exist. Error: %v", err)
		return
	}
	if downloaderImage, ok := cm.Data["model_serving_downloader_image"]; ok {
		config.Config.SetDownloaderImage(downloaderImage)
	} else {
		klog.Warning("Failed to load Downloader Image. Use Default Value.")
	}
	if runtimeImage, ok := cm.Data["model_serving_runtime_image"]; ok {
		config.Config.SetRuntimeImage(runtimeImage)
	} else {
		klog.Warning("Failed to load Runtime Image. Use Default Value.")
	}
}

// When model serving status changed, model reconciles
func (mc *ModelBoosterController) triggerModel(old any, new any) {
	modelServing, ok := new.(*workload.ModelServing)
	if !ok {
		klog.Error("failed to parse new ModelServing")
		return
	}
	_, ok = old.(*workload.ModelServing)
	if !ok {
		klog.Error("failed to parse old ModelServing")
		return
	}
	if len(modelServing.OwnerReferences) > 0 {
		// Find the owner of modelServing and reconcile the owner to change its status
		if model, err := mc.modelBoosterLister.ModelBoosters(modelServing.Namespace).Get(modelServing.OwnerReferences[0].Name); err == nil {
			mc.enqueueModelBooster(model)
		}
	}
}

// deleteModelServing is called when a ModelServing is deleted. It will reconcile the ModelBooster. Recreate model serving.
func (mc *ModelBoosterController) deleteModelServing(obj any) {
	modelServing, ok := obj.(*workload.ModelServing)
	if !ok {
		klog.Error("failed to parse ModelServing when deleteModelServing")
		return
	}
	klog.V(4).Infof("model serving: %s is deleted", klog.KObj(modelServing))
	if len(modelServing.OwnerReferences) > 0 {
		if model, err := mc.modelBoosterLister.ModelBoosters(modelServing.Namespace).Get(modelServing.OwnerReferences[0].Name); err == nil {
			mc.enqueueModelBooster(model)
		}
	}
}

// deleteModelRoute is called when a ModelRoute is deleted. It will reconcile the ModelBooster. Recreate model route.
func (mc *ModelBoosterController) deleteModelRoute(obj any) {
	modelRoute, ok := obj.(*networkingv1alpha1.ModelRoute)
	if !ok {
		klog.Error("failed to parse ModelRoute when deleteModelRoute")
		return
	}
	klog.V(4).Infof("model route: %s is deleted", klog.KObj(modelRoute))
	if len(modelRoute.OwnerReferences) > 0 {
		if model, err := mc.modelBoosterLister.ModelBoosters(modelRoute.Namespace).Get(modelRoute.OwnerReferences[0].Name); err == nil {
			mc.enqueueModelBooster(model)
		}
	}
}

// deleteModelServer is called when a ModelServer is deleted. It will reconcile the ModelBooster. Recreate model server.
func (mc *ModelBoosterController) deleteModelServer(obj any) {
	modelServer, ok := obj.(*networkingv1alpha1.ModelServer)
	if !ok {
		klog.Error("failed to parse ModelServer when deleteModelServer")
		return
	}
	klog.V(4).Infof("model server: %s is deleted", klog.KObj(modelServer))
	if len(modelServer.OwnerReferences) > 0 {
		if model, err := mc.modelBoosterLister.ModelBoosters(modelServer.Namespace).Get(modelServer.OwnerReferences[0].Name); err == nil {
			mc.enqueueModelBooster(model)
		}
	}
}
