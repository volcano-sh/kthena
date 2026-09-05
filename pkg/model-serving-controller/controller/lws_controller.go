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
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	lwsclientset "sigs.k8s.io/lws/client-go/clientset/versioned"
	lwsinformers "sigs.k8s.io/lws/client-go/informers/externalversions"
	lwslisters "sigs.k8s.io/lws/client-go/listers/leaderworkerset/v1"

	kthenaclientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	kthenainformers "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	kthenalisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	msplugins "github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

func InitializeLWSController(
	cfg *rest.Config,
	kubeClient kubernetes.Interface,
	kthenaClient kthenaclientset.Interface,
) (*LWSController, error) {
	exists, err := ResourceExists(kubeClient, "leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet")
	if err != nil {
		return nil, fmt.Errorf("failed to check LWS CRD existence: %v", err)
	}
	if !exists {
		return nil, nil
	}

	lwsClient, err := lwsclientset.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create lws client: %v", err)
	}

	lwsInformerFactory := lwsinformers.NewSharedInformerFactory(lwsClient, 0)
	kthenaInformerFactory := kthenainformers.NewSharedInformerFactory(kthenaClient, 0)

	controller, err := NewLWSController(kubeClient, kthenaClient, lwsClient, lwsInformerFactory, kthenaInformerFactory)
	if err != nil {
		return nil, fmt.Errorf("failed to create LWS controller: %v", err)
	}

	return controller, nil
}

// LWSController reconciles a LeaderWorkerSet object
type LWSController struct {
	kubeClient            kubernetes.Interface
	kthenaClient          kthenaclientset.Interface
	lwsClient             lwsclientset.Interface
	kubeInformerFactory   kubeinformers.SharedInformerFactory
	lwsInformerFactory    lwsinformers.SharedInformerFactory
	kthenaInformerFactory kthenainformers.SharedInformerFactory
	serviceSynced         cache.InformerSynced
	lwsLister             lwslisters.LeaderWorkerSetLister
	lwsSynced             cache.InformerSynced
	modelServingLister    kthenalisters.ModelServingLister
	modelServingSynced    cache.InformerSynced

	workqueue workqueue.TypedRateLimitingInterface[string]
}

func NewLWSController(
	kubeClient kubernetes.Interface,
	kthenaClient kthenaclientset.Interface,
	lwsClient lwsclientset.Interface,
	lwsInformer lwsinformers.SharedInformerFactory,
	kthenaInformer kthenainformers.SharedInformerFactory,
) (*LWSController, error) {
	selector, err := labels.NewRequirement(lwsv1.SetNameLabelKey, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("create LWS service selector: %w", err)
	}
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(
		kubeClient,
		0,
		kubeinformers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = selector.String()
		}),
	)
	serviceInformerInstance := kubeInformerFactory.Core().V1().Services()
	lwsInformerInstance := lwsInformer.Leaderworkerset().V1().LeaderWorkerSets()
	modelServingInformerInstance := kthenaInformer.Workload().V1alpha1().ModelServings()

	c := &LWSController{
		kubeClient:            kubeClient,
		kthenaClient:          kthenaClient,
		lwsClient:             lwsClient,
		kubeInformerFactory:   kubeInformerFactory,
		lwsInformerFactory:    lwsInformer,
		kthenaInformerFactory: kthenaInformer,
		serviceSynced:         serviceInformerInstance.Informer().HasSynced,
		lwsLister:             lwsInformerInstance.Lister(),
		lwsSynced:             lwsInformerInstance.Informer().HasSynced,
		modelServingLister:    modelServingInformerInstance.Lister(),
		modelServingSynced:    modelServingInformerInstance.Informer().HasSynced,
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "LeaderWorkerSets"},
		),
	}

	klog.Info("Setting up event handlers for LWS Controller")
	_, err = lwsInformerInstance.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueLWS,
		UpdateFunc: func(old, new interface{}) {
			c.enqueueLWS(new)
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = modelServingInformerInstance.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*workloadv1alpha1.ModelServing)
			oldDepl := old.(*workloadv1alpha1.ModelServing)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				return
			}
			c.handleObject(new)
		},
		DeleteFunc: c.handleObject,
	})
	if err != nil {
		return nil, err
	}

	_, err = serviceInformerInstance.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: c.handleObject,
	})
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *LWSController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("Starting LWS controller")

	c.kubeInformerFactory.Start(ctx.Done())
	c.lwsInformerFactory.Start(ctx.Done())
	c.kthenaInformerFactory.Start(ctx.Done())

	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.serviceSynced, c.lwsSynced, c.modelServingSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	klog.Info("Started workers")
	<-ctx.Done()
	klog.Info("Shutting down workers")

	return nil
}

func (c *LWSController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *LWSController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	err := func(key string) error {
		defer c.workqueue.Done(key)
		if err := c.syncHandler(ctx, key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(key)
		return nil
	}(key)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *LWSController) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	lws, err := c.lwsLister.LeaderWorkerSets(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("lws '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}
	if err := c.ensureLWSHeadlessService(ctx, lws); err != nil {
		return err
	}

	msName := lws.Name
	ms, err := c.modelServingLister.ModelServings(namespace).Get(msName)
	if errors.IsNotFound(err) {
		ms = c.constructModelServing(lws)
		_, err = c.kthenaClient.WorkloadV1alpha1().ModelServings(namespace).Create(ctx, ms, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		// Wait for the ModelServing informer to observe the created object before
		// projecting its status back to the LWS.
		return nil
	} else if err != nil {
		return err
	} else {
		desiredMs := c.constructModelServing(lws)
		if !reflect.DeepEqual(ms.Spec, desiredMs.Spec) {
			msCopy := ms.DeepCopy()
			msCopy.Spec = desiredMs.Spec
			ms, err = c.kthenaClient.WorkloadV1alpha1().ModelServings(namespace).Update(ctx, msCopy, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			// Admission defaults can make the stored spec differ from the
			// constructed spec without changing its generation. Only wait when the
			// update actually made the ModelServing status stale.
			if ms.Status.ObservedGeneration != ms.Generation {
				return nil
			}
		}
	}

	if err := c.updateLWSStatus(ctx, lws, ms); err != nil {
		return err
	}

	return nil
}

func (c *LWSController) enqueueLWS(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *LWSController) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		klog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	klog.V(4).Infof("Processing object: %s", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		if ownerRef.Kind != "LeaderWorkerSet" {
			return
		}

		lws, err := c.lwsLister.LeaderWorkerSets(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			klog.V(4).Infof("ignoring orphaned object '%s' of lws '%s'", object.GetSelfLink(), ownerRef.Name)
			return
		}

		c.enqueueLWS(lws)
		return
	}
}

func (c *LWSController) constructModelServing(lws *lwsv1.LeaderWorkerSet) *workloadv1alpha1.ModelServing {
	replicas := int32(1)
	if lws.Spec.Replicas != nil {
		replicas = *lws.Spec.Replicas
	}

	convertTemplate := func(src corev1.PodTemplateSpec) workloadv1alpha1.PodTemplateSpec {
		return workloadv1alpha1.PodTemplateSpec{
			Metadata: &workloadv1alpha1.Metadata{
				Labels:      src.ObjectMeta.Labels,
				Annotations: src.ObjectMeta.Annotations,
			},
			Spec: src.Spec,
		}
	}

	convertTemplatePtr := func(src *corev1.PodTemplateSpec) *workloadv1alpha1.PodTemplateSpec {
		if src == nil {
			return nil
		}
		t := convertTemplate(*src)
		return &t
	}

	workerSize := int32(1)
	if lws.Spec.LeaderWorkerTemplate.Size != nil {
		workerSize = *lws.Spec.LeaderWorkerTemplate.Size
	}
	workerReplicas := max(workerSize-1, 0)

	roleReplicas := int32(1)

	var leaderTemplate corev1.PodTemplateSpec
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderTemplate = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate
	} else {
		leaderTemplate = lws.Spec.LeaderWorkerTemplate.WorkerTemplate
	}

	role := workloadv1alpha1.Role{
		Name:           "default",
		Replicas:       &roleReplicas,
		EntryTemplate:  convertTemplate(leaderTemplate),
		WorkerReplicas: workerReplicas,
		WorkerTemplate: convertTemplatePtr(&lws.Spec.LeaderWorkerTemplate.WorkerTemplate),
	}

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(lws, lwsv1.GroupVersion.WithKind("LeaderWorkerSet")),
			},
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: &replicas,
			Plugins: []workloadv1alpha1.PluginSpec{
				{
					Name: msplugins.LWSLabelsPluginName,
					Type: workloadv1alpha1.PluginTypeBuiltIn,
				},
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{role},
			},
		},
	}
	return ms
}

func (c *LWSController) updateLWSStatus(ctx context.Context, lws *lwsv1.LeaderWorkerSet, ms *workloadv1alpha1.ModelServing) error {
	newStatus := buildLWSStatus(lws, ms, metav1.Now())

	if !reflect.DeepEqual(lws.Status, newStatus) {
		lwsCopy := lws.DeepCopy()
		lwsCopy.Status = newStatus
		_, err := c.lwsClient.LeaderworkersetV1().LeaderWorkerSets(lws.Namespace).UpdateStatus(ctx, lwsCopy, metav1.UpdateOptions{})
		return err
	}
	return nil
}

func buildLWSStatus(lws *lwsv1.LeaderWorkerSet, ms *workloadv1alpha1.ModelServing, now metav1.Time) lwsv1.LeaderWorkerSetStatus {
	newStatus := *lws.Status.DeepCopy()
	newStatus.Replicas = ms.Status.Replicas
	newStatus.ReadyReplicas = ms.Status.AvailableReplicas
	newStatus.UpdatedReplicas = ms.Status.UpdatedReplicas
	newStatus.Conditions = projectLWSConditions(lws, ms, now)
	return newStatus
}

func (c *LWSController) ensureLWSHeadlessService(ctx context.Context, lws *lwsv1.LeaderWorkerSet) error {
	_, err := c.kubeClient.CoreV1().Services(lws.Namespace).Get(ctx, lws.Name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Labels: map[string]string{
				lwsv1.SetNameLabelKey: lws.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(lws, lwsv1.GroupVersion.WithKind("LeaderWorkerSet")),
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector: map[string]string{
				lwsv1.SetNameLabelKey: lws.Name,
			},
		},
	}
	if _, err := c.kubeClient.CoreV1().Services(lws.Namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create LWS headless service: %w", err)
	}
	return nil
}

func projectLWSConditions(lws *lwsv1.LeaderWorkerSet, ms *workloadv1alpha1.ModelServing, now metav1.Time) []metav1.Condition {
	managedTypes := map[string]struct{}{
		string(lwsv1.LeaderWorkerSetAvailable):        {},
		string(lwsv1.LeaderWorkerSetProgressing):      {},
		string(lwsv1.LeaderWorkerSetUpdateInProgress): {},
	}

	conditions := make([]metav1.Condition, 0, len(ms.Status.Conditions))
	for _, condition := range lws.Status.Conditions {
		if _, managed := managedTypes[condition.Type]; !managed {
			conditions = append(conditions, condition)
		}
	}

	sourceConditions := ms.Status.Conditions
	if ms.Status.ObservedGeneration != ms.Generation {
		sourceConditions = append([]metav1.Condition(nil), sourceConditions...)
		availableFound := false
		for i := range sourceConditions {
			if sourceConditions[i].Type == string(workloadv1alpha1.ModelServingAvailable) {
				sourceConditions[i].Status = metav1.ConditionFalse
				sourceConditions[i].Reason = "ModelServingStatusStale"
				sourceConditions[i].Message = "ModelServing has not observed its latest generation"
				availableFound = true
			}
		}
		if !availableFound {
			sourceConditions = append(sourceConditions, metav1.Condition{
				Type:    string(workloadv1alpha1.ModelServingAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  "ModelServingStatusStale",
				Message: "ModelServing has not observed its latest generation",
			})
		}
	}

	for _, condition := range sourceConditions {
		if _, managed := managedTypes[condition.Type]; !managed {
			continue
		}
		projected := condition
		projected.ObservedGeneration = lws.Generation
		for _, previous := range lws.Status.Conditions {
			if previous.Type != projected.Type {
				continue
			}
			if previous.Status == projected.Status {
				projected.LastTransitionTime = previous.LastTransitionTime
			} else {
				projected.LastTransitionTime = now
			}
			break
		}
		if projected.LastTransitionTime.IsZero() {
			projected.LastTransitionTime = now
		}
		conditions = append(conditions, projected)
	}
	return conditions
}

func ResourceExists(client kubernetes.Interface, groupVersion string, kind string) (bool, error) {
	resources, err := client.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}
