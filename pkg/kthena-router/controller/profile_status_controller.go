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
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// ProfileBuildStatusReader reports the profile's build outcome and signals
// when it changes. Declared here so this package does not import scheduler.
type ProfileBuildStatusReader interface {
	BuildStatus(key types.NamespacedName) (built bool, buildErr error)
	// StatusNotify signals that at least one profile's build status changed.
	StatusNotify() <-chan struct{}
	// TakePendingNotifies returns and clears the profiles whose build status
	// changed.
	TakePendingNotifies() []types.NamespacedName
}

// ProfileStatusController reports the resolution state of a ModelServer's
// referenced SchedulerProfile on the ModelServer's status. It observes
// ModelServer events and the build-outcome notifications, and is the single
// writer of the SchedulerProfileResolved condition.
type ProfileStatusController struct {
	modelServerLister      listerv1alpha1.ModelServerLister
	schedulerProfileLister listerv1alpha1.SchedulerProfileLister

	modelServerSynced      cache.InformerSynced
	schedulerProfileSynced cache.InformerSynced

	// buildStatus reports the chain build outcome and signals its changes.
	buildStatus ProfileBuildStatusReader

	kthenaClient clientset.Interface

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
}

func NewProfileStatusController(
	kthenaInformerFactory informersv1alpha1.SharedInformerFactory,
	buildStatus ProfileBuildStatusReader,
	kthenaClient clientset.Interface,
) (*ProfileStatusController, error) {
	modelServerInformer := kthenaInformerFactory.Networking().V1alpha1().ModelServers()
	schedulerProfileInformer := kthenaInformerFactory.Networking().V1alpha1().SchedulerProfiles()

	controller := &ProfileStatusController{
		modelServerLister:      modelServerInformer.Lister(),
		schedulerProfileLister: schedulerProfileInformer.Lister(),
		modelServerSynced:      modelServerInformer.Informer().HasSynced,
		schedulerProfileSynced: schedulerProfileInformer.Informer().HasSynced,
		buildStatus:            buildStatus,
		kthenaClient:           kthenaClient,
		workqueue:              workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:            &atomic.Bool{},
	}

	_, err := modelServerInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueModelServer,
		UpdateFunc: func(_, new interface{}) {
			controller.enqueueModelServer(new)
		},
		DeleteFunc: controller.enqueueModelServer,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add modelserver event handler for profile status controller: %w", err)
	}

	return controller, nil
}

func (c *ProfileStatusController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.modelServerSynced, c.schedulerProfileSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)
	go c.consumeStatusNotify(stopCh)

	<-stopCh
	return nil
}

func (c *ProfileStatusController) HasSynced() bool {
	return c.initialSync.Load()
}

// consumeStatusNotify re-enqueues referencing ModelServers whenever a
// profile's build status changes: the async chain build may finish after a
// sync already skipped reporting as in-flight, and nothing else re-triggers
// those ModelServers.
func (c *ProfileStatusController) consumeStatusNotify(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case <-c.buildStatus.StatusNotify():
			for _, key := range c.buildStatus.TakePendingNotifies() {
				c.enqueueModelServersReferencingProfile(key.Namespace, key.Name)
			}
		}
	}
}

func (c *ProfileStatusController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ProfileStatusController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial modelServer and schedulerProfile resources have been synced")
		c.workqueue.Forget(obj)
		c.initialSync.Store(true)
		return true
	}

	key, ok := obj.(string)
	if !ok {
		c.workqueue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	err := c.syncHandler(key)
	retryOrForget(c.workqueue, "profileStatus", key, err, klog.V(2).Infof)
	return true
}

func (c *ProfileStatusController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	ms, err := c.modelServerLister.ModelServers(namespace).Get(name)
	if errors.IsNotFound(err) {
		return nil // the ModelServer is gone; its status went with it
	}
	if err != nil {
		return err
	}

	return c.reportSchedulerProfileCondition(ms)
}

// reportSchedulerProfileCondition reports the referenced SchedulerProfile's
// resolution state on the ModelServer's status.
func (c *ProfileStatusController) reportSchedulerProfileCondition(ms *aiv1alpha1.ModelServer) error {
	if c.kthenaClient == nil || c.buildStatus == nil {
		return nil
	}
	if ms.Spec.TrafficPolicy == nil ||
		ms.Spec.TrafficPolicy.LoadBalancer == nil ||
		ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile == nil {
		// The reference is gone: the ModelServer now serves on the global
		// chain, so the condition no longer applies and must not linger.
		return c.clearSchedulerProfileCondition(ms)
	}
	ref := *ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile
	profileKey := types.NamespacedName{Namespace: ms.Namespace, Name: ref}

	built, buildErr := c.buildStatus.BuildStatus(profileKey)
	if !built && buildErr == nil {
		// The chain build runs in an async store callback, so a just-created
		// profile may not have finished building when this sync runs. A
		// profile that the lister knows about is in-flight, not missing —
		// keep the current condition and let the status-change signal
		// re-trigger reporting.
		if _, err := c.schedulerProfileLister.SchedulerProfiles(ms.Namespace).Get(ref); err == nil {
			return nil
		}
	}

	var status metav1.ConditionStatus
	var reason, message string
	switch {
	case buildErr != nil:
		status = metav1.ConditionFalse
		reason = aiv1alpha1.SchedulerProfileResolvedReasonResolutionFailed
		message = fmt.Sprintf("SchedulerProfile %q failed to build: %v (the previously built configuration, if any, is still serving)", profileKey.String(), buildErr)
	case !built:
		status = metav1.ConditionFalse
		reason = aiv1alpha1.SchedulerProfileResolvedReasonNotFound
		message = fmt.Sprintf("SchedulerProfile %q not found", profileKey.String())
	default:
		status = metav1.ConditionTrue
		reason = aiv1alpha1.SchedulerProfileResolvedReasonResolved
		message = fmt.Sprintf("SchedulerProfile %q is resolved and serving", profileKey.String())
	}
	cond := metav1.Condition{
		Type:    aiv1alpha1.SchedulerProfileResolvedConditionType,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
	updated := ms.DeepCopy()
	apimeta.SetStatusCondition(&updated.Status.Conditions, cond)
	if reflect.DeepEqual(ms.Status, updated.Status) {
		return nil
	}
	// Log on the state transition only: an unchanged condition would
	// otherwise re-log (and re-trigger a sync via the status update) on
	// every pass.
	if status != metav1.ConditionTrue {
		klog.Warningf("ModelServer %s/%s: SchedulerProfile unresolved: %s", ms.Namespace, ms.Name, message)
	} else {
		klog.V(4).Infof("ModelServer %s/%s: SchedulerProfile resolved", ms.Namespace, ms.Name)
	}
	_, err := c.kthenaClient.NetworkingV1alpha1().ModelServers(ms.Namespace).
		UpdateStatus(context.TODO(), updated, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		return nil // the ModelServer is gone;
	}
	if err != nil {
		klog.Warningf("ModelServer %s/%s: failed to report SchedulerProfile condition: %v",
			ms.Namespace, ms.Name, err)
		return err
	}
	return nil
}

// clearSchedulerProfileCondition removes the SchedulerProfileResolved
// condition when the ModelServer no longer references a SchedulerProfile.
func (c *ProfileStatusController) clearSchedulerProfileCondition(ms *aiv1alpha1.ModelServer) error {
	if apimeta.FindStatusCondition(ms.Status.Conditions, aiv1alpha1.SchedulerProfileResolvedConditionType) == nil {
		return nil
	}
	updated := ms.DeepCopy()
	apimeta.RemoveStatusCondition(&updated.Status.Conditions, aiv1alpha1.SchedulerProfileResolvedConditionType)
	_, err := c.kthenaClient.NetworkingV1alpha1().ModelServers(ms.Namespace).
		UpdateStatus(context.TODO(), updated, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		klog.Warningf("ModelServer %s/%s: failed to clear SchedulerProfile condition: %v",
			ms.Namespace, ms.Name, err)
		return err
	}
	return nil
}

func (c *ProfileStatusController) enqueueModelServer(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	c.workqueue.Add(key)
}

// enqueueModelServersReferencingProfile re-queues every ModelServer referencing
// the profile, so their conditions are re-reported after its status changed.
func (c *ProfileStatusController) enqueueModelServersReferencingProfile(namespace, name string) {
	servers, err := c.modelServerLister.ModelServers(namespace).List(labels.Everything())
	if err != nil {
		return
	}
	for _, ms := range servers {
		if ms.Spec.TrafficPolicy == nil ||
			ms.Spec.TrafficPolicy.LoadBalancer == nil ||
			ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile == nil {
			continue
		}
		if *ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile == name {
			klog.V(4).Infof("SchedulerProfile %s/%s: changed, re-enqueueing ModelServer %s/%s",
				namespace, name, ms.Namespace, ms.Name)
			c.enqueueModelServer(ms)
		}
	}
}
