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
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextClientSet "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcano "volcano.sh/apis/pkg/client/clientset/versioned"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	// enqueueAfter is the time duration to wait to re-enqueue:
	enqueueAfter = 1 * time.Second

	GroupNameKey = "GroupName"
	RoleIDKey    = "RoleID"
)

// PodGroupManager is the interface for managing PodGroups.
// This interface allows for dependency injection in tests.
type PodGroupManager interface {
	CreateOrUpdatePodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, pgName string) (error, time.Duration)
	DeletePodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) error
	CleanupPodGroups(ctx context.Context, ms *workloadv1alpha1.ModelServing) error
	HasPodGroupCRD() bool
	GetPodGroupInformer() cache.SharedIndexInformer
	Run(parentCtx context.Context) error
	GenerateTaskName(roleName string, roleIndex int) string
	AnnotatePodWithPodGroup(pod *corev1.Pod, ms *workloadv1alpha1.ModelServing, groupName, taskName string)
}

type podGracePeriodKey struct {
	types.NamespacedName
	UID types.UID
}

func getPodGracePeriodKey(pod *corev1.Pod) podGracePeriodKey {
	return podGracePeriodKey{
		NamespacedName: utils.GetNamespaceName(pod),
		UID:            pod.UID,
	}
}

type ModelServingController struct {
	kubeClientSet      kubernetes.Interface
	modelServingClient clientset.Interface

	syncHandler           func(ctx context.Context, msKey string) error
	podGroupManager       PodGroupManager
	podsLister            listerv1.PodLister
	podsInformer          cache.SharedIndexInformer
	servicesLister        listerv1.ServiceLister
	servicesInformer      cache.SharedIndexInformer
	modelServingLister    listerv1alpha1.ModelServingLister
	modelServingsInformer cache.SharedIndexInformer

	// nolint
	workqueue       workqueue.RateLimitingInterface
	store           datastore.Store
	graceMap        sync.Map    // key: podGracePeriodKey, value:time
	initialSync     atomic.Bool // indicates whether the initial sync has been completed
	pluginsRegistry *plugins.Registry
	recorder        record.EventRecorder
	podDeletions    sync.Map // key: auxiliaryCleanupKey; value: *podDeletionTargets
	roleCleanups    sync.Map // key: auxiliaryCleanupKey; value: *plugins.RoleCleanup
}

func NewModelServingController(kubeClientSet kubernetes.Interface, modelServingClient clientset.Interface, volcanoClient volcano.Interface, apiextClient apiextClientSet.Interface) (*ModelServingController, error) {
	selector, err := labels.NewRequirement(workloadv1alpha1.GroupNameLabelKey, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create label selector, err: %v", err)
	}

	// Register ModelServing types in the global scheme for event recording.
	if err := workloadv1alpha1.Install(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to register ModelServing API scheme: %v", err)
	}

	kubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		kubeClientSet,
		0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = selector.String()
		}),
	)
	podsInformer := kubeInformerFactory.Core().V1().Pods()
	servicesInformer := kubeInformerFactory.Core().V1().Services()
	modelServingInformerFactory := informersv1alpha1.NewSharedInformerFactory(modelServingClient, 0)
	modelServingInformer := modelServingInformerFactory.Workload().V1alpha1().ModelServings()

	err = podsInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create pod Informer Index, err: %v", err)
	}

	err = servicesInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create service Informer Index, err: %v", err)
	}

	store := datastore.New()

	// setup event broadcaster & recorder
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: kubeClientSet.CoreV1().Events(""),
	})
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "modelserving-controller"},
	)

	c := &ModelServingController{
		kubeClientSet:         kubeClientSet,
		modelServingClient:    modelServingClient,
		podGroupManager:       nil,
		podsLister:            podsInformer.Lister(),
		podsInformer:          podsInformer.Informer(),
		servicesLister:        servicesInformer.Lister(),
		servicesInformer:      servicesInformer.Informer(),
		modelServingLister:    modelServingInformer.Lister(),
		modelServingsInformer: modelServingInformer.Informer(),
		// nolint
		workqueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ModelServings"),
		store:           store,
		pluginsRegistry: plugins.DefaultRegistry,
		recorder:        recorder,
	}

	registerPodGroupHandler := func(pgInformer cache.SharedIndexInformer) {
		if c == nil || pgInformer == nil {
			return
		}
		_, _ = pgInformer.AddEventHandler(cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				metaObj := getMetaObject(obj)
				if metaObj == nil {
					return false
				}
				return isOwnedByModelServing(metaObj)
			},
			Handler: cache.ResourceEventHandlerFuncs{
				DeleteFunc: func(obj interface{}) {
					c.deletePodGroup(obj)
				},
			},
		})
	}

	c.podGroupManager = podgroupmanager.NewManager(kubeClientSet, volcanoClient, apiextClient, registerPodGroupHandler)

	klog.Info("Set the ModelServing event handler")
	_, _ = c.modelServingsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.addModelServing(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.updateModelServing(oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			c.deleteModelServing(obj)
		},
	})

	_, _ = c.podsInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			metaObj := getMetaObject(obj)
			if metaObj == nil {
				return false
			}
			return isOwnedByModelServing(metaObj)
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				c.addPod(obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				c.updatePod(oldObj, newObj)
			},
			DeleteFunc: func(obj interface{}) {
				c.deletePod(obj)
			},
		},
	})

	_, _ = c.servicesInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			metaObj := getMetaObject(obj)
			if metaObj == nil {
				return false
			}
			return isOwnedByModelServing(metaObj)
		},
		Handler: cache.ResourceEventHandlerFuncs{
			DeleteFunc: func(obj interface{}) {
				c.deleteService(obj)
			},
		},
	})

	c.syncHandler = c.syncModelServing

	return c, nil
}

func (c *ModelServingController) addModelServing(obj interface{}) {
	ms, ok := obj.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Errorf("failed to parse ModelServing %#v", obj)
		return
	}
	klog.V(4).InfoS("Adding", "modelServing", klog.KObj(ms))
	c.enqueueModelServing(ms)
}

func (c *ModelServingController) updateModelServing(old, cur interface{}) {
	curms, ok := cur.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Errorf("failed to parse new ModelServing type when update %#v", cur)
		return
	}
	oldms, ok := old.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Errorf("failed to parse old ModelServing type when update %#v", old)
		return
	}

	if reflect.DeepEqual(oldms.Spec, curms.Spec) {
		// If the spec has not changed, we do not need to reconcile.
		klog.V(4).InfoS("Spec has not changed, skipping update", "modelServing", klog.KObj(curms))
		return
	}

	// If network topology is removed, we need to clean up the PodGroups.
	// Because minRoleReplicas is not allowed to be updated, so we do not need to check it here.
	if oldms.Spec.Template.NetworkTopology != nil && curms.Spec.Template.NetworkTopology == nil {
		if curms.Spec.Template.GangPolicy == nil || len(curms.Spec.Template.GangPolicy.MinRoleReplicas) == 0 {
			if err := c.podGroupManager.CleanupPodGroups(context.TODO(), curms); err != nil {
				klog.Errorf("failed to clean up PodGroups for ModelServing %s/%s: %v", curms.Namespace, curms.Name, err)
			}
		}
	}

	c.enqueueModelServing(curms)
}

func (c *ModelServingController) deleteModelServing(obj interface{}) {
	ms, ok := obj.(*workloadv1alpha1.ModelServing)
	if !ok {
		// If the object is not a ModelServing, it might be a tombstone object.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("failed to parse ModelServing type when delete %#v", obj)
			return
		}
		ms, ok = tombstone.Obj.(*workloadv1alpha1.ModelServing)
		if !ok {
			klog.Errorf("failed to parse ModelServing from tombstone %#v", tombstone.Obj)
			return
		}
	}

	c.roleCleanups.Range(func(key, _ any) bool {
		if key.(auxiliaryCleanupKey).uid == ms.UID {
			c.roleCleanups.Delete(key)
		}
		return true
	})
	c.podDeletions.Range(func(key, _ any) bool {
		if key.(auxiliaryCleanupKey).uid == ms.UID {
			c.podDeletions.Delete(key)
		}
		return true
	})
	c.store.DeleteModelServing(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	})
	// ControllerRevisions will be automatically deleted via OwnerReference when ModelServing is deleted
}

func (c *ModelServingController) addPod(obj interface{}) {
	c.updatePod(nil, obj)
}

func (c *ModelServingController) updatePod(_, newObj interface{}) {
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		klog.Error("failed to parse newPod type when updatePod")
		return
	}
	klog.V(4).Infof("updatePod: %s/%s, %v", newPod.Namespace, newPod.Name, newPod.Status.Phase)

	if newPod.DeletionTimestamp != nil {
		// If the pod is being deleted, we do not need to handle it.
		// After deleted，following work will be done in deletePod.
		return
	}

	ms, servingGroupName, err := c.getModelServingByChildResource(newPod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("modelServing of pod %s has been deleted", newPod.Name)
		} else {
			klog.Errorf("get model Serving failed when update pod %s/%s: %v", newPod.Namespace, newPod.Name, err)
		}
		return
	}

	if c.shouldSkipHandling(ms, servingGroupName, newPod) {
		return
	}

	inPlaceState := inplace.StateOf(newPod)
	switch {
	case inPlaceState.InProgress() && (utils.IsPodFailed(newPod) || inPlaceState.UnexpectedRestart(newPod)):
		// A crash during an in-place update is not covered by the expected restarts and must be recovered.
		klog.V(4).Infof("handleErrorPod: %s/%s", newPod.Namespace, newPod.Name)
		err = c.handleErrorPod(ms, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle error pod %s/%s failed for ModelServing %s/%s: %v", newPod.Namespace, newPod.Name, ms.Namespace, ms.Name, err)
		}
	case inPlaceState.Updating(newPod):
		c.handleInPlaceUpdatingPod(ms, servingGroupName, newPod)
	case utils.IsPodRunningAndReady(newPod):
		klog.V(4).Infof("handleReadyPod: %s/%s", newPod.Namespace, newPod.Name)
		if inPlaceState.InProgress() {
			// Every patched container restarted: let the rollout finish the update.
			defer c.enqueueModelServing(ms)
		}
		// The pod is available, that is, the state is running, and the container is ready
		err = c.handleReadyPod(ms, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle running pod %s/%s failed for ModelServing %s/%s: %v", newPod.Namespace, newPod.Name, ms.Namespace, ms.Name, err)
		}
	case utils.IsPodFailed(newPod) || inPlaceState.UnexpectedRestart(newPod):
		klog.V(4).Infof("handleErrorPod: %s/%s", newPod.Namespace, newPod.Name)
		err = c.handleErrorPod(ms, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle error pod %s/%s failed for ModelServing %s/%s: %v", newPod.Namespace, newPod.Name, ms.Namespace, ms.Name, err)
		}
	default:
		if roleCoordination(ms) != nil {
			c.enqueueModelServing(ms)
		}
		klog.V(4).Infof("handleDefault: %s/%s", newPod.Namespace, newPod.Name)
		c.addServingGroupAndRoleFromPod(ms, servingGroupName, newPod)
	}
}

func (c *ModelServingController) addServingGroupAndRoleFromPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) {
	if c.initialSync.Load() {
		return
	}
	roleName := utils.GetRoleName(pod)
	roleTemplateHash := c.resolveRoleTemplateHash(ms, roleName, pod)
	c.store.AddServingGroupAndRole(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	}, servingGroupName, utils.ObjectRevision(pod), roleTemplateHash, roleName, utils.GetRoleID(pod))
}

// handleInPlaceUpdatingPod keeps a Pod that is being updated in place out of the
// Running set. Its containers restart on purpose, so it must not be recovered.
func (c *ModelServingController) handleInPlaceUpdatingPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) {
	c.addServingGroupAndRoleFromPod(ms, servingGroupName, pod)
	if err := c.markPodUnavailable(ms, servingGroupName, pod); err != nil {
		klog.Warningf("mark in-place updating pod %s/%s unavailable: %v", pod.Namespace, pod.Name, err)
	}
	c.enqueueModelServing(ms)
}

func (c *ModelServingController) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// If the object is not a Pod, it might be a tombstone object.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Error("failed to parse pod type when deletePod")
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("failed to parse Pod from tombstone %#v", tombstone.Obj)
			return
		}
	}

	ms, servingGroupName, roleName, roleID := c.getModelServingAndResourceDetails(pod)
	// ms is nil means the modelserving is deleted
	// delete the pod
	if ms == nil {
		klog.Warningf("ModelServing of deleted pod: %s not found, might be already deleted", pod.Name)
		return
	}

	// Remove the pod from running pods in the store
	c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), servingGroupName, pod.Name)

	// skip handling if pod revision mismatches serving group revision or owner mismatch
	if c.shouldSkipHandling(ms, servingGroupName, pod) {
		return
	}

	if c.handleDeletionInProgress(ms, servingGroupName, roleName, roleID) {
		return
	}

	err := c.handleDeletedPod(ms, servingGroupName, pod)
	if err != nil {
		klog.Errorf("handle deleted pod %s/%s failed in ServingGroup %s of ModelServing %s/%s: %v", pod.Namespace, pod.Name, servingGroupName, ms.Namespace, ms.Name, err)
	}
}

func (c *ModelServingController) deleteService(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		// If the object is not a Service, it might be a tombstone object.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Error("failed to parse service type when deleteService")
			return
		}
		svc, ok = tombstone.Obj.(*corev1.Service)
		if !ok {
			klog.Errorf("failed to parse Service from tombstone %#v", tombstone.Obj)
			return
		}
	}

	ms, servingGroupName, roleName, roleID := c.getModelServingAndResourceDetails(svc)
	// ms is nil means the modelserving is deleted
	if ms == nil {
		klog.Warningf("ModelServing of deleted service: %s not found, might be already deleted", svc.Name)
		return
	}

	// skip handling if service revision mismatches serving group revision or owner mismatch
	if c.shouldSkipHandling(ms, servingGroupName, svc) {
		return
	}

	if c.handleDeletionInProgress(ms, servingGroupName, roleName, roleID) {
		return
	}

	klog.V(4).Infof("Service %s/%s deleted, enqueuing ModelServing %s for reconcile", svc.GetNamespace(), svc.GetName(), ms.Name)
	c.enqueueModelServing(ms)
}

func (c *ModelServingController) deletePodGroup(obj interface{}) {
	pg, ok := obj.(*schedulingv1beta1.PodGroup)
	if !ok {
		// If the object is not a PodGroup, it might be a tombstone object.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Error("failed to parse podgroup type when deletePodGroup")
			return
		}
		pg, ok = tombstone.Obj.(*schedulingv1beta1.PodGroup)
		if !ok {
			klog.Errorf("failed to parse PodGroup from tombstone %#v", tombstone.Obj)
			return
		}
	}

	ms, servingGroupName, _, _ := c.getModelServingAndResourceDetails(pg)
	// ms is nil means the modelserving is deleted
	if ms == nil {
		klog.Warningf("ModelServing of deleted podGroup: %s not found, might be already deleted", pg.Name)
		return
	}

	// skip handling if podGroup revision mismatches serving group revision or owner mismatch
	if c.shouldSkipHandling(ms, servingGroupName, pg) {
		return
	}

	// If servingGroup is deleting, skip handling.
	if c.handleDeletionInProgress(ms, servingGroupName, "", "") {
		return
	}

	klog.V(4).Infof("podGroup %s/%s deleted, enqueuing ModelServing %s for reconcile", pg.GetNamespace(), pg.GetName(), ms.Name)
	c.enqueueModelServing(ms)
}

func (c *ModelServingController) enqueueModelServing(ms *workloadv1alpha1.ModelServing) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(ms); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *ModelServingController) enqueueModelServingAfter(ms *workloadv1alpha1.ModelServing, duration time.Duration) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(ms); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.AddAfter(key, duration)
}

func (c *ModelServingController) worker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *ModelServingController) processNextWorkItem(ctx context.Context) bool {
	key, quit := c.workqueue.Get()
	if quit {
		return false
	}
	defer c.workqueue.Done(key)

	err := c.syncHandler(ctx, key.(string))
	if err == nil {
		c.workqueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	c.workqueue.AddRateLimited(key)

	return true
}

func (c *ModelServingController) syncModelServing(ctx context.Context, key string) (syncErr error) {
	klog.V(4).InfoS("Started syncing ModelServing", "key", key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", err)
	}

	ms, err := c.modelServingLister.ModelServings(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		klog.V(4).Infof("%v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	// Recover orphan cleanup independently of migration errors in surviving Roles.
	orphanErr := c.cleanupOrphanServices(ctx, ms)
	defer func() { syncErr = errors.Join(syncErr, orphanErr) }()
	if err := c.finishPendingDeletions(ctx, ms); err != nil {
		return err
	}
	ctx = c.withRevisionHistory(ctx, ms)
	ctx = withCoordinatedRound(ctx, ms)
	history := c.revisionHistory(ctx, ms)
	revision, err := history.desiredRevision(ctx)
	if err != nil {
		// Revision persistence is required before creating or replacing Pods. An
		// explicit replica decrease is safe without a new template, so retain that
		// path and return the revision error to keep workqueue retries active.
		return errors.Join(fmt.Errorf("resolve desired revision: %w", err), c.scaleDownOnRevisionError(ctx, ms))
	}
	if err := c.persistCoordinatedRoleRevision(ctx, ms, revision); err != nil {
		return fmt.Errorf("failed to persist coordinated Role revision: %v", err)
	}

	// Explicit scale-down does not need the discarded groups' old templates.
	if roleCoordination(ms) != nil {
		groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
		if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
			return err
		}
		if len(groups) > modelServingReplicas(ms) {
			if err := c.scaleDownServingGroups(ctx, ms, groups, modelServingReplicas(ms)); err != nil {
				return err
			}
		}
	}
	if err := c.refreshCoordinatedAvailabilityInRound(ctx, ms); err != nil {
		return err
	}

	// 1. Sync the number of ServingGroups to match the expected replicas defined in spec.
	if err := c.syncServingGroupReplicas(ctx, ms, revision); err != nil {
		return fmt.Errorf("failed to sync ServingGroup replicas: %v", err)
	}

	// Derive optional dependency and proportional limits from the current Role state.
	rolloutPolicy, err := c.resolveRoleRolloutPolicy(ctx, ms, revision)
	if err != nil {
		return fmt.Errorf("failed to resolve Role rollout policy: %v", err)
	}

	// 2. Sync the roles and their replicas within each ServingGroup, handling partitioned scaling and revisions.
	roleSyncErr := c.syncRoleReplicas(ctx, ms, revision, rolloutPolicy)
	if roleSyncErr != nil && !onlyRevisionErrors(roleSyncErr) {
		return fmt.Errorf("failed to sync role replicas: %w", roleSyncErr)
	}

	// 3. Handle the rolling update process, deleting outdated ServingGroups/Roles to trigger updates.
	if err := c.manageRollingUpdateWithPolicy(ctx, ms, revision, rolloutPolicy); err != nil {
		return fmt.Errorf("failed to handle rollingUpdate: %v", err)
	}

	// 4. Calculate and update the overall condition and replica status fields of the ModelServing.
	if err := c.updateModelServingStatusInRound(ctx, ms, revision, rolloutPolicy); err != nil {
		return fmt.Errorf("failed to update status of ms %s/%s: %v", namespace, name, err)
	}

	// Unknown history never authorizes a rollout. Returning the cached errors
	// after safe reconciliation makes the workqueue retry without blocking an
	// explicit scale-down that already completed above.
	return errors.Join(roleSyncErr, history.errors())
}

func (c *ModelServingController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// start informers
	go c.podsInformer.RunWithContext(ctx)
	go c.servicesInformer.RunWithContext(ctx)
	go c.modelServingsInformer.RunWithContext(ctx)

	if err := c.podGroupManager.Run(ctx); err != nil {
		klog.Errorf("failed to start PodGroup informer: %v", err)
	}

	cache.WaitForCacheSync(ctx.Done(),
		c.podsInformer.HasSynced,
		c.servicesInformer.HasSynced,
		c.modelServingsInformer.HasSynced,
	)

	// sync pods first
	c.syncAll()
	klog.Info("initial sync has been done")

	klog.Info("start modelServing controller")
	for i := 0; i < workers; i++ {
		go c.worker(ctx)
	}
	<-ctx.Done()
	klog.Info("shut down modelServing controller")
}

func (c *ModelServingController) syncAll() {
	pods, err := c.podsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods {
		c.addPod(pod)
	}

	modelServings, err := c.modelServingLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list model servings: %v", err)
	}
	for _, ms := range modelServings {
		c.addModelServing(ms)
	}

	c.initialSync.Store(true)
}

// syncServingGroupReplicas scales up or down whole ServingGroups to meet the top-level
// `Spec.Replicas` count of the ModelServing resource.
// Main processing steps:
// 1. Retrieve the list of active ServingGroups for the current ModelServing from the data store.
// 2. Compare the current count of ServingGroups with the expected replicas.
// 3. If scaling up: sequentially initialize needed group states, create PodGroups, and scale up groups.
// 4. If scaling down: sort the groups by priority/deletion-cost and delete the excess groups.
func (c *ModelServingController) syncServingGroupReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, newRevision string) error {
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get servingGroup of modelServing: %s from map: %v", ms.GetName(), err)
	}
	replicas := modelServingReplicas(ms)
	expectedCount := replicas
	isServingGroupRollingUpdate := ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate
	if isServingGroupRollingUpdate {
		maxSurge, err := utils.GetMaxSurge(ms)
		if err != nil {
			return fmt.Errorf("failed to calculate maxSurge: %v", err)
		}
		partition, _, partitionErr := c.getPartition(modelServingPartition(ms), replicas)
		if partitionErr != nil {
			return fmt.Errorf("failed to calculate partition: %v", partitionErr)
		}
		if maxSurge > 0 && !c.updatesInPlace(ms) && c.hasUpdateableOutdatedServingGroup(ctx, ms, servingGroupList, newRevision, partition) {
			expectedCount += maxSurge
		}
	}

	curReplicas := len(servingGroupList)
	if curReplicas < expectedCount {
		klog.V(2).Infof("manageServingGroupReplicas: scaling up modelServing=%s (%d -> %d)", utils.GetNamespaceName(ms), curReplicas, expectedCount)
		if err := c.scaleUpServingGroups(ctx, ms, servingGroupList, expectedCount, newRevision); err != nil {
			return fmt.Errorf("failed to scale up ServingGroups: %v", err)
		}
	} else if curReplicas > expectedCount {
		klog.V(2).Infof("manageServingGroupReplicas: scaling down modelServing=%s (%d -> %d)", utils.GetNamespaceName(ms), curReplicas, expectedCount)
		if err := c.scaleDownServingGroups(ctx, ms, servingGroupList, expectedCount); err != nil {
			return fmt.Errorf("failed to scale down ServingGroups: %v", err)
		}
	}

	// Note: in case the role is updated, we need to update pod groups as well.
	// update pod group after scaling down, so that we do not need to update pod group for deleting serving groups
	// Moreover, it is also possible to reconstruct accidentally deleted podGroups here.
	servingGroupList, err = c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot refresh ServingGroups of modelServing %s: %v", ms.GetName(), err)
	}
	for _, servingGroup := range servingGroupList {
		if servingGroup.Status != datastore.ServingGroupDeleting {
			workload := ms
			if c.podGroupManager.HasPodGroupCRD() && roleCoordination(ms) == nil {
				workload, err = c.workloadForPodGroup(ctx, ms, servingGroup)
				if err != nil {
					// Do not overwrite the applied gang requirements with a target
					// layout when history is unavailable. Reconcile returns this
					// cached error after processing independent safe actions.
					continue
				}
			}
			if err := c.createOrUpdatePodGroupByServingGroup(ctx, workload, servingGroup.Name); err != nil {
				return fmt.Errorf("failed to update PodGroup for ServingGroup %s: %v", servingGroup.Name, err)
			}
		}
	}
	return nil
}

// hasUpdateableOutdatedServingGroup derives the temporary capacity requirement
// from observed ServingGroups. Ordinals identify ServingGroups but do not
// determine whether a group is surge capacity: binpack scale-down can leave
// sparse or high ordinals in the normal replica set.
func (c *ModelServingController) hasUpdateableOutdatedServingGroup(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	groups []datastore.ServingGroup,
	revision string,
	partition int,
) bool {
	for _, group := range groups {
		if !partitionProtected(group.Name, partition) && c.compareServingGroupTemplate(ctx, ms, group, revision) == templateDifferent {
			return true
		}
	}
	return false
}

// scaleDownOnRevisionError performs only replica-count decreases. It never
// creates Pods or treats a hash mismatch as permission to replace a template.
// This keeps the safe part of reconciliation available while an immutable
// ControllerRevision is temporarily missing, invalid, or conflicting.
func (c *ModelServingController) scaleDownOnRevisionError(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	var errs []error
	expectedGroups := modelServingReplicas(ms)
	isServingGroupRollingUpdate := ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate
	if expectedGroups > 0 && isServingGroupRollingUpdate && !c.updatesInPlace(ms) {
		// An unresolved target cannot prove that rollout surge is obsolete.
		// Retain its configured allowance until history is readable again, while
		// still removing capacity above that bound and allowing scale-to-zero.
		maxSurge, err := utils.GetMaxSurge(ms)
		if err == nil && maxSurge < 0 {
			err = fmt.Errorf("maxSurge must not be negative")
		}
		if err != nil {
			errs = append(errs, err)
			expectedGroups = len(groups)
		} else {
			expectedGroups += maxSurge
		}
	}
	if len(groups) > expectedGroups {
		if err := c.scaleDownServingGroups(ctx, ms, groups, expectedGroups); err != nil {
			errs = append(errs, err)
		}
	}
	// An excess coordinated Role may be healthy surge capacity, not an
	// explicit replica decrease. Without a resolved target, do not bypass the
	// coordinator's retirement and availability checks. Whole-group shrink
	// above remains independent of the discarded groups' revision history.
	if roleCoordination(ms) != nil {
		return errors.Join(errs...)
	}
	partition, _, partitionErr := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	if partitionErr != nil {
		errs = append(errs, partitionErr)
		partition = 0
	}
	for _, group := range groups {
		status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group.Name)
		if status == datastore.ServingGroupDeleting || status == datastore.ServingGroupNotFound {
			continue
		}

		rolesToScale := ms.Spec.Template.Roles
		if partitionProtected(group.Name, partition) {
			historical, historyErr := c.revisionHistory(ctx, ms).roles(ctx, group.Revision)
			if historyErr != nil {
				errs = append(errs, fmt.Errorf("resolve protected ServingGroup %s: %w", group.Name, historyErr))
				continue
			}
			rolesToScale = historical
		}
		for _, role := range rolesToScale {
			instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, role.Name)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			expected := roleReplicas(role)
			if expected > 0 && !isServingGroupRollingUpdate && !c.roleUpdatesInPlace(ms, role.Name) {
				maxSurge, err := utils.GetMaxSurgeForRole(role)
				if err == nil && maxSurge < 0 {
					err = fmt.Errorf("maxSurge for Role %s must not be negative", role.Name)
				}
				if err != nil {
					errs = append(errs, err)
					continue
				}
				expected += maxSurge
			}
			if len(instances) > expected {
				if err := c.scaleDownRoles(ctx, ms, group.Name, role, instances, expected); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// scaleUpServingGroups fills missing ordinals in [0, expectedCount).
// Missing ordinals below partition use CurrentRevision; the rest use newRevision.
func (c *ModelServingController) scaleUpServingGroups(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupList []datastore.ServingGroup, expectedCount int, newRevision string) error {
	partition, _, err := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	if err != nil {
		return err
	}
	existingOrdinals := make([]int, 0, len(servingGroupList))
	for _, group := range servingGroupList {
		_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
		if ordinal >= 0 {
			existingOrdinals = append(existingOrdinals, ordinal)
		}
	}
	var scaleUpErr error
	forEachMissingOrdinal(expectedCount, existingOrdinals, max(0, expectedCount-len(servingGroupList)), func(ordinal int) bool {
		revision := newRevision
		if ordinal < partition && ms.Status.CurrentRevision != "" {
			revision = ms.Status.CurrentRevision
		}
		// Persistence happens before reconciliation. Both desired and protected
		// workloads must resolve their immutable snapshot before creating Pods.
		workload, err := c.revisionHistory(ctx, ms).workload(ctx, revision)
		if err != nil {
			scaleUpErr = fmt.Errorf("resolve revision %s for ServingGroup ordinal %d: %w", revision, ordinal, err)
			return false
		}
		groupName := utils.GenerateServingGroupName(ms.Name, ordinal)
		if err := c.createOrUpdatePodGroupByServingGroup(ctx, workload, groupName); err != nil {
			scaleUpErr = err
			return false
		}
		if err := c.CreatePodsForServingGroup(ctx, workload, ordinal, revision, workload.Spec.Template.Roles); err != nil {
			scaleUpErr = err
			return false
		}
		c.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, revision)
		return true
	})
	return scaleUpErr
}

// syncRoleReplicas applies current Role counts while preserving the applied
// template of each existing ServingGroup. A missing Role in SG mode is a local
// repair, even outside partition; Role mode resolves each instance separately.
func (c *ModelServingController) syncRoleReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, newRevision string, rolloutPolicy *roleRolloutPolicy) error {
	ctx = c.withRevisionHistory(ctx, ms)
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return err
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return err
	}
	var errs []error
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
		roles, revision := ms.Spec.Template.Roles, newRevision
		if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
			revision = group.Revision
			roles, err = c.revisionHistory(ctx, ms).roles(ctx, revision)
			if err != nil {
				errs = append(errs, err)
				// The cached history error is returned after safe reconciliation.
				// This unit cannot prove readiness or authorize its own rollout,
				// but must not block independent units with sufficient budget.
				if group.Status == datastore.ServingGroupRunning {
					errs = append(errs, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group.Name, datastore.ServingGroupCreating))
				}
				// Keep the existing safe nested count-decrease path. Role surge is
				// inactive in SG mode; do not reclaim whole-group surge on this error.
				partition, _, partitionErr := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
				if partitionErr == nil && ordinal >= partition {
					for _, role := range ms.Spec.Template.Roles {
						instances, listErr := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, role.Name)
						if listErr == nil && len(instances) > roleReplicas(role) {
							if err := c.scaleDownRoles(ctx, ms, group.Name, role, instances, roleReplicas(role)); err != nil {
								errs = append(errs, err)
							}
						}
					}
				}
				continue
			}
			present, presenceErr := c.unitHasPods(ctx, ms, group.Name, "")
			if presenceErr != nil {
				errs = append(errs, presenceErr)
				continue
			}
			if !present {
				partition, _, partitionErr := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
				if partitionErr != nil {
					errs = append(errs, partitionErr)
					continue
				}
				recreatedRevision := newRevision
				if ordinal < partition {
					recreatedRevision = ms.Status.CurrentRevision
				}
				if recreatedRevision != revision {
					workload, historyErr := c.revisionHistory(ctx, ms).workload(ctx, recreatedRevision)
					if historyErr != nil {
						errs = append(errs, historyErr)
						continue
					}
					// The whole unit may select a different worker layout. Align
					// its gang requirements before creating the replacement Pods.
					if err := c.createOrUpdatePodGroupByServingGroup(ctx, workload, group.Name); err != nil {
						errs = append(errs, err)
						continue
					}
					// No live member (including terminating Pods) remains. Reset
					// only the in-memory observation before creating the whole unit;
					// partial CREATE success is then associated with its new history.
					c.store.DeleteServingGroup(utils.GetNamespaceName(ms), group.Name)
					c.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, recreatedRevision)
					errs = append(errs, c.CreatePodsForServingGroup(ctx, workload, ordinal, recreatedRevision, workload.Spec.Template.Roles))
					continue
				}
			}
		}
		for _, role := range roles {
			if err := c.manageRoleReplicasPerGroup(ctx, ms, group.Name, role, ordinal, revision, rolloutPolicy.allowTargetStart(group.Name, role.Name), chain); err != nil {
				errs = append(errs, err)
			}
		}
		ready, readinessErr := c.checkServingGroupReadyWithContext(ctx, ms, group.Name)
		if readinessErr != nil {
			errs = append(errs, readinessErr)
			continue
		}
		status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group.Name)
		if status != datastore.ServingGroupDeleting {
			if ready {
				errs = append(errs, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group.Name, datastore.ServingGroupRunning))
			} else if status == datastore.ServingGroupRunning {
				errs = append(errs, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group.Name, datastore.ServingGroupCreating))
			}
		}
	}
	return errors.Join(errs...)
}

// scaleDownRoles handles Role scaling down with two-level priority-based selection:
// 1. Primary: Not-ready roles (Creating, NotFound) are deleted first
// 2. Secondary: Among roles with same status, lower deletion cost = delete first
// Partition protects templates, not explicit replica-count decreases.
func (c *ModelServingController) scaleDownRoles(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, roleList []datastore.Role, expectedCount int) error {
	// Calculate priority information for all Roles
	allScores := make([]RoleWithScore, 0, len(roleList))
	for _, role := range roleList {
		if role.Status == datastore.RoleDeleting {
			// Skip roles that are already being deleted
			continue
		}
		scoreInfo := c.calculateRoleScore(ms, groupName, targetRole.Name, role.Name)
		allScores = append(allScores, scoreInfo)
	}

	if len(allScores) <= expectedCount {
		klog.V(4).Infof("No need to scale down role %s in ServingGroup %s: current count=%d, expected count=%d", targetRole.Name, groupName, len(allScores), expectedCount)
		return nil
	}

	// Sort by priority tuple: (priority, deletionCost, index).
	// Lower priority value = higher deletion priority (delete first)
	// Lower deletion cost = higher deletion priority
	// Higher index = higher deletion priority (backward compatibility)
	sortRoles := func(a, b RoleWithScore) int {
		// Primary: Sort by priority (not-ready first)
		if a.Priority != b.Priority {
			return cmp.Compare(a.Priority, b.Priority) // Ascending: lower priority (not-ready) first
		}

		// Secondary: Among roles with same priority, lower deletion cost comes first
		if a.DeletionCost != b.DeletionCost {
			return cmp.Compare(a.DeletionCost, b.DeletionCost) // Ascending: lower cost first
		}

		// Tertiary: Higher index comes first (backward compatibility)
		return cmp.Compare(b.Index, a.Index) // Descending: higher indices first
	}

	slices.SortFunc(allScores, sortRoles)

	totalToDelete := max(0, len(allScores)-expectedCount)

	// Role needs to scale down, and the ServingGroup status needs to be set to Scaling
	err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, datastore.ServingGroupScaling)
	klog.V(4).Infof("Setting ServingGroup %s/%s status to Scaling for role %s scaling down", ms.Namespace+"/"+ms.Name, groupName, targetRole.Name)
	if err != nil {
		klog.Errorf("failed to set ServingGroup %s/%s status: %v", ms.Namespace+"/"+ms.Name, groupName, err)
		return err
	}

	for i := 0; i < totalToDelete; i++ {
		target := allScores[i]
		klog.V(2).Infof("Scaling down role %s (priority: %d, deletion cost: %d, index: %d)",
			target.Name, target.Priority, target.DeletionCost, target.Index)
		if err := c.DeleteRole(ctx, ms, groupName, targetRole.Name, target.Name); err != nil {
			return err
		}
	}

	return nil
}

// scaleUpRoles fills missing Role ordinals in [0, expectedCount).
// Missing ordinals below partition use CurrentRevision; the rest use newRevision.
// Dependency coordination may temporarily pause newRevision creation.
func (c *ModelServingController) scaleUpRoles(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	groupName string,
	targetRole workloadv1alpha1.Role,
	roleList []datastore.Role,
	expectedCount int,
	servingGroupOrdinal int,
	newRevision string,
	allowTargetStart bool,
) error {
	partition, partitionConfigured, partitionErr := c.getPartition(rolePartition(ms, targetRole), roleReplicas(targetRole))
	if partitionErr != nil {
		klog.Errorf("scaleUpRoles: failed to parse partition for role %s: %v", targetRole.Name, partitionErr)
	}

	existingOrdinals := make([]int, 0, len(roleList))
	for _, role := range roleList {
		_, ordinal := utils.GetParentNameAndOrdinal(role.Name)
		if ordinal < 0 {
			klog.Warningf("scaleUpRoles: cannot parse ordinal from Role %s", role.Name)
			continue
		}
		existingOrdinals = append(existingOrdinals, ordinal)
	}
	stableSlots := 0
	if roleCoordination(ms) != nil {
		physical, err := c.getPodsByIndex(GroupNameKey, ms.Namespace+"/"+groupName)
		if err != nil {
			c.enqueueModelServingAfter(ms, enqueueAfter)
			return err
		}
		occupied := sets.New(existingOrdinals...)
		for _, pod := range physical {
			if utils.IsOwnedByModelServingWithUID(pod, ms.UID) && utils.GetRoleName(pod) == targetRole.Name {
				_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(pod))
				if ordinal >= 0 {
					occupied.Insert(ordinal)
				}
			}
		}
		existingOrdinals = sets.List(occupied)
		marked, err := c.surgePodsInRound(ctx, ms, surgeRole, groupName, targetRole.Name)
		if err != nil {
			c.enqueueModelServingAfter(ms, enqueueAfter)
			return err
		}
		stableSlots = max(0, roleReplicas(targetRole)-(len(roleList)-markedSurgeNames(marked, surgeRole, roleReplicas(targetRole)).Len()))
	}
	toCreate := max(0, expectedCount-len(existingOrdinals))

	// Role needs to scale up, and the ServingGroup status needs to be set to Scaling
	err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, datastore.ServingGroupScaling)
	klog.V(4).Infof("Setting ServingGroup %s/%s status to Scaling for role %s scaling up", ms.Namespace+"/"+ms.Name, groupName, targetRole.Name)
	if err != nil {
		klog.Errorf("failed to set ServingGroup %s/%s status: %v", ms.Namespace+"/"+ms.Name, groupName, err)
		return err
	}

	// Helper function to create a Role
	createRole := func(ordinal int, revision string) error {
		workload, err := c.revisionHistory(ctx, ms).workload(ctx, revision)
		if err != nil {
			return err
		}
		roleTemplateHash, err := utils.RoleRevisionHash(workload, targetRole.Name)
		if err != nil {
			return err
		}
		// Create pods for role
		roleToApply, err := historicalRole(workload, targetRole.Name, revision)
		if err != nil {
			return err
		}
		createCtx, err := c.coordinatedCreationContext(ctx, ms, groupName, targetRole, roleToApply, ordinal, revision)
		if err != nil {
			return err
		}
		if roleCoordination(ms) != nil && ordinal >= roleReplicas(targetRole) {
			createCtx = context.WithValue(createCtx, coordinatedSurgeKey{}, true)
		}
		appliedHash, err := c.createPodsByRole(createCtx, *roleToApply.DeepCopy(), ms, ordinal, servingGroupOrdinal, revision, roleTemplateHash)
		if err != nil {
			return fmt.Errorf("create role %s for ServingGroup %s failed: %v", utils.GenerateRoleID(targetRole.Name, ordinal), groupName, err)
		}
		// Insert new Role to global storage
		roleID := utils.GenerateRoleID(targetRole.Name, ordinal)
		c.store.AddRole(utils.GetNamespaceName(ms), groupName, targetRole.Name, roleID, revision, appliedHash)
		if roleCoordination(ms) != nil {
			c.enqueueModelServingAfter(ms, enqueueAfter)
		}
		c.enqueueModelServing(ms)
		// Emit event for new role entering Creating state
		message := fmt.Sprintf("Role %s/%s in ServingGroup %s is now Creating", targetRole.Name, roleID, groupName)
		c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "RoleCreating", message)
		return nil
	}

	var scaleUpErr error
	createMissing := func(ordinal int) bool {
		if partitionConfigured && partition > 0 && ordinal < partition {
			if roleCoordination(ms) != nil {
				baseline := ms.Status.CurrentRevision
				if baseline == "" {
					baseline = newRevision
				}
				old, err := c.roleSpecsFromRevisionInRound(ctx, ms, baseline)
				if err != nil {
					scaleUpErr = err
					c.enqueueModelServingAfter(ms, enqueueAfter)
					return false
				}
				_, ok := old[targetRole.Name]
				if !ok {
					scaleUpErr = fmt.Errorf("historical ControllerRevision %s has no Role %s", baseline, targetRole.Name)
					c.enqueueModelServingAfter(ms, enqueueAfter)
					return false
				}
				if err := createRole(ordinal, baseline); err != nil {
					scaleUpErr = err
					c.enqueueModelServingAfter(ms, enqueueAfter)
					return false
				}
				return true
			}
			// Use CurrentRevision for partition-protected ordinals
			revisionToUse := newRevision
			if ms.Status.CurrentRevision != "" {
				revisionToUse = ms.Status.CurrentRevision
			}
			// Observed protected replicas can still pin an older revision. Never
			// guess by taking an arbitrary item from retained history.
			if revisionToUse == newRevision {
				for _, observed := range roleList {
					if partitionProtected(observed.Name, partition) && observed.Revision != "" && observed.Revision != newRevision {
						if revisionToUse != newRevision && revisionToUse != observed.Revision {
							c.revisionHistory(ctx, ms).recordComparisonError(groupName+"/"+targetRole.Name,
								fmt.Errorf("ambiguous historical revision for missing protected Role ordinal %d", ordinal))
							return false
						}
						revisionToUse = observed.Revision
					}
				}
			}
			klog.V(4).Infof("scaleUpRoles: ordinal %d missing (partition-protected), revisionToUse=%s, currentRevision=%s",
				ordinal, revisionToUse, ms.Status.CurrentRevision)

			if err := createRole(ordinal, revisionToUse); err != nil {
				scaleUpErr = fmt.Errorf("scaleUpRoles: failed to create role %s at ordinal %d in ServingGroup %s of ModelServing %s/%s: %w", targetRole.Name, ordinal, groupName, ms.Namespace, ms.Name, err)
				return false
			}
			return true
		}
		if !allowTargetStart {
			// Dependency startup gating applies to every target-version creation,
			// including stable capacity added by an ordinary scale-up.
			return false
		}
		if err := createRole(ordinal, newRevision); err != nil {
			scaleUpErr = fmt.Errorf("scaleUpRoles: failed to create role %s at ordinal %d in ServingGroup %s of ModelServing %s/%s: %w", targetRole.Name, ordinal, groupName, ms.Namespace, ms.Name, err)
			if roleCoordination(ms) != nil {
				c.enqueueModelServingAfter(ms, enqueueAfter)
			}
			return false
		}
		return true
	}
	if roleCoordination(ms) != nil {
		forEachCoordinatedOrdinal(roleReplicas(targetRole), stableSlots, existingOrdinals, toCreate, createMissing)
	} else {
		forEachMissingOrdinal(expectedCount, existingOrdinals, toCreate, createMissing)
	}
	return scaleUpErr
}

// manageRoleReplicasPerGroup manages the replicas of a specific role within an Serving group
// It handles both scale up and scale down operations for the role
func (c *ModelServingController) manageRoleReplicasPerGroup(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	groupName string,
	targetRole workloadv1alpha1.Role,
	servingGroupOrdinal int,
	newRevision string,
	allowTargetStart bool,
	chain *plugins.Chain,
) (syncErr error) {
	ctx = c.withRevisionHistory(ctx, ms)
	var historyErrors []error
	defer func() { syncErr = errors.Join(append(historyErrors, syncErr)...) }()
	// TODO: add podGroup update after gang scheduler finished
	// Get all replicas of a role from storage, for example, prefill-0, prefill-1...
	roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, targetRole.Name)
	if err != nil {
		return fmt.Errorf("manageRoleReplicasPerGroup: cannot get role %s in ServingGroup %s: %w", targetRole.Name, groupName, err)
	}

	if roleCoordination(ms) != nil {
		if err := c.adoptCoordinatedSurge(ctx, ms, groupName, targetRole); err != nil {
			c.enqueueModelServingAfter(ms, enqueueAfter)
			return err
		}
	}
	expectedCount := roleReplicas(targetRole)
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate &&
		!c.roleUpdatesInPlace(ms, targetRole.Name) && c.hasUpdateableOutdatedRoleInRound(ctx, ms, groupName, targetRole, roleList) {
		maxSurge, err := utils.GetMaxSurgeForRole(targetRole)
		if err != nil {
			klog.Errorf("manageRoleReplicasPerGroup: failed to calculate maxSurge for role %s in ServingGroup %s: %v", targetRole.Name, groupName, err)
		} else {
			expectedCount += maxSurge
		}
	}
	// Explicit shrink remains possible even when legacy DNS blocks Role sync.
	// Marked temporary surge must keep the coordinator's identity and budget checks.
	if len(roleList) > expectedCount {
		retired := false
		if roleCoordination(ms) != nil {
			retired, err = c.retireCoordinatedSurge(ctx, ms, groupName, targetRole, roleList, expectedCount)
			if err != nil {
				return err
			}
		}
		if !retired {
			if err := c.scaleDownRoles(ctx, ms, groupName, targetRole, roleList, expectedCount); err != nil {
				return err
			}
		}
		roleList, err = c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, targetRole.Name)
		if err != nil {
			return err
		}
	}
	for _, roleObj := range roleList {
		if roleCoordination(ms) != nil {
			live, _, err := c.coordinatedLiveRole(ctx, ms, groupName, targetRole.Name, roleObj)
			if err != nil {
				return err
			}
			roleObj = live
		}
		if roleObj.Status == datastore.RoleDeleting {
			continue
		}
		roleIDValue := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, targetRole.Name, roleObj.Name)
		pods, err := c.getPodsByIndex(RoleIDKey, roleIDValue)
		if err != nil {
			klog.Warningf("manageRoleReplicasPerGroup: failed to list pods for role %s/%s in ServingGroup %s: %v", targetRole.Name, roleObj.Name, groupName, err)
			continue
		}
		if roleCoordination(ms) != nil && ms.ResourceVersion != "" {
			pods, err = c.coordinatedPods(ctx, ms, labels.Set{workloadv1alpha1.GroupNameLabelKey: groupName, workloadv1alpha1.RoleLabelKey: targetRole.Name, workloadv1alpha1.RoleIDKey: roleObj.Name}.AsSelector())
			if err != nil {
				return err
			}
		}
		roleToApply, revisionToUse, hashToUse, err := c.appliedRoleTemplate(ctx, ms, groupName, targetRole.Name, roleObj)
		if err != nil {
			klog.Warningf("manageRoleReplicasPerGroup: cannot resolve template for role %s/%s: %v", targetRole.Name, roleObj.Name, err)
			c.revisionHistory(ctx, ms).recordComparisonError(groupName+"/"+roleObj.Name, err)
			historyErrors = append(historyErrors, err)
			if roleObj.Status == datastore.RoleRunning {
				if _, statusErr := c.markRoleStatus(ms, groupName, targetRole.Name, roleObj.Name, datastore.RoleCreating); statusErr != nil {
					klog.Warningf("cannot invalidate readiness of Role %s/%s: %v", targetRole.Name, roleObj.Name, statusErr)
				}
			}
			continue
		}
		if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
			present, presenceErr := c.unitHasPods(ctx, ms, groupName, roleObj.Name)
			if presenceErr != nil {
				c.revisionHistory(ctx, ms).recordComparisonError(groupName+"/"+roleObj.Name, presenceErr)
				continue
			}
			if !present {
				partition, _, partitionErr := c.getPartition(rolePartition(ms, targetRole), roleReplicas(targetRole))
				if partitionErr != nil {
					c.revisionHistory(ctx, ms).recordComparisonError(roleObj.Name, partitionErr)
					continue
				}
				_, ordinal := utils.GetParentNameAndOrdinal(roleObj.Name)
				recreated := roleObj
				recreated.Revision = newRevision
				if ordinal < partition {
					recreated.Revision = ms.Status.CurrentRevision
				}
				if recreated.Revision == newRevision && !allowTargetStart {
					continue
				}
				roleToApply, revisionToUse, hashToUse, err = c.appliedRoleTemplate(ctx, ms, groupName, targetRole.Name, recreated)
				if err != nil {
					continue
				}
				if revisionToUse != roleObj.Revision {
					c.store.DeleteRole(utils.GetNamespaceName(ms), groupName, targetRole.Name, roleObj.Name)
					c.store.AddRole(utils.GetNamespaceName(ms), groupName, targetRole.Name, roleObj.Name, revisionToUse, hashToUse)
					// Refresh gang requirements even if the replacement stays Pending
					// or CREATE only partially succeeds before informer observation.
					c.enqueueModelServing(ms)
				}
			}
		}
		applicable, err := c.modelServingForRevision(ctx, ms, revisionToUse)
		if err != nil {
			return err
		}
		applicableChain, err := c.buildPluginChain(applicable)
		if err != nil {
			return err
		}
		ready := rolePodsComplete(ms, groupName, roleObj.Name, roleToApply, revisionToUse, pods, true)
		// roleObj is a copy read before the Pods were listed. Compare against it, not the
		// live status, so a Role that a Pod handler marked Running after the list was taken
		// is not moved back to Creating from this older Pod list.
		var statusErr error
		if ready && roleObj.Status != datastore.RoleRunning {
			_, statusErr = c.markRoleStatus(ms, groupName, targetRole.Name, roleObj.Name, datastore.RoleRunning)
		} else if !ready && roleObj.Status == datastore.RoleRunning {
			_, statusErr = c.markRoleStatus(ms, groupName, targetRole.Name, roleObj.Name, datastore.RoleCreating)
		}
		if statusErr != nil {
			klog.Warningf("cannot update readiness of Role %s/%s: %v", targetRole.Name, roleObj.Name, statusErr)
			continue
		}
		expectedPods := 1 + int(roleToApply.WorkerReplicas)
		if !rolePodsComplete(ms, groupName, roleObj.Name, roleToApply, revisionToUse, pods, false) {
			klog.V(2).Infof("manageRoleReplicasPerGroup: role %s/%s in ServingGroup %s is missing pods (%d/%d), recreating", targetRole.Name, roleObj.Name, groupName, len(pods), expectedPods)
			_, roleIndex := utils.GetParentNameAndOrdinal(roleObj.Name)
			if err := c.createPodsByRoleWithContext(ctx, roleToApply, applicable, roleIndex, servingGroupOrdinal, revisionToUse, hashToUse, applicableChain); err != nil {
				return fmt.Errorf("manageRoleReplicasPerGroup: failed to recreate pods for role %s/%s in ServingGroup %s: %w", targetRole.Name, roleObj.Name, groupName, err)
			}
		}
		if applicableChain != nil {
			_, roleIndex := utils.GetParentNameAndOrdinal(roleObj.Name)
			if err := applicableChain.OnRoleSync(ctx, &plugins.HookRequest{
				ModelServing:  applicable,
				ServingGroup:  groupName,
				RoleName:      targetRole.Name,
				RoleID:        roleObj.Name,
				RoleIndex:     roleIndex,
				Role:          roleToApply.DeepCopy(),
				KubeClient:    c.kubeClientSet,
				ServiceLister: c.servicesLister,
			}); err != nil {
				return fmt.Errorf("sync plugins for Role %s/%s in ServingGroup %s: %w", targetRole.Name, roleObj.Name, groupName, err)
			}
		}
	}

	if roleCoordination(ms) != nil && !c.hasUpdateableOutdatedRoleInRound(ctx, ms, groupName, targetRole, roleList) {
		if handled, err := c.finishCoordinatedSurge(ctx, ms, groupName, targetRole, roleList, servingGroupOrdinal, newRevision, allowTargetStart); handled || err != nil {
			return err
		}
	}

	// Determine whether it is a scale-up or scale-down scenario
	if len(roleList) < expectedCount {
		klog.V(2).Infof("manageRoleReplicasPerGroup: scaling UP role %s in ServingGroup %s: current=%d, expected=%d", targetRole.Name, groupName, len(roleList), expectedCount)
		return c.scaleUpRoles(ctx, ms, groupName, targetRole, roleList, expectedCount, servingGroupOrdinal, newRevision, allowTargetStart)
	}
	return nil
}

// hasUpdateableOutdatedRole reports whether a Role has an outdated replica
// outside its absolute partition boundary. maxSurge changes only the temporary
// expected replica count; individual replicas are not classified as surge by
// ordinal because binpack scale-down may leave sparse or high ordinals.
func (c *ModelServingController) hasUpdateableOutdatedRoleInRound(ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	groupName string,
	targetRole workloadv1alpha1.Role,
	roleList []datastore.Role,
) bool {
	partition, _, err := c.getPartition(rolePartition(ms, targetRole), roleReplicas(targetRole))
	if err != nil {
		klog.Errorf("hasUpdateableOutdatedRole: failed to calculate partition for role %s in ServingGroup %s: %v", targetRole.Name, groupName, err)
		return false
	}
	for _, role := range roleList {
		if partitionProtected(role.Name, partition) || role.Status == datastore.RoleDeleting {
			continue
		}
		if roleCoordination(ms) != nil {
			if c.compareRoleTemplate(ctx, ms, datastore.ServingGroup{Name: groupName}, targetRole.Name, role) == templateDifferent {
				return true
			}
			continue
		}
		if c.compareRoleTemplate(ctx, ms, datastore.ServingGroup{
			Name:     groupName,
			Revision: role.Revision,
		}, targetRole.Name, role) == templateDifferent {
			return true
		}
	}
	return false
}

// emitRoleStatusEvent emits a Kubernetes Event for a role-related status change.
// It is intentionally lightweight and no-op when recorder is not initialized.
func (c *ModelServingController) emitRoleStatusEvent(
	ms *workloadv1alpha1.ModelServing,
	eventType, reason, message string,
) {
	if c == nil || c.recorder == nil || ms == nil {
		return
	}
	c.recorder.Event(ms, eventType, reason, message)
}

// markRoleStatus moves a role to Running or Creating and, only if this call made the
// change, sends the RoleRunning or RoleCreating Event, so the Pod handlers, the sync
// loop and the coordinated availability refresh send one Event per change between
// them. It never requeues: the sync and the refresh run inside a sync whose later
// steps already see the change, and a later step can move the role back, as an
// in-place update does, so a requeue there would restart the sync again and again.
func (c *ModelServingController) markRoleStatus(ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string, status datastore.RoleStatus) (bool, error) {
	changed, err := c.store.TransitionRoleStatus(utils.GetNamespaceName(ms), groupName, roleName, roleID, status)
	if err != nil || !changed {
		return false, err
	}
	klog.V(2).Infof("Update role %s/%s status to %s", roleName, roleID, status)
	message := fmt.Sprintf("Role %s/%s in ServingGroup %s is now %s", roleName, roleID, groupName, status)
	c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "Role"+string(status), message)
	return true, nil
}

func (c *ModelServingController) getModelServingAndResourceDetails(resource metav1.Object) (*workloadv1alpha1.ModelServing, string, string, string) {
	ms, servingGroupName, err := c.getModelServingByChildResource(resource)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("modelServing of svc %s/%s has been deleted", resource.GetNamespace(), resource.GetName())
		} else {
			klog.Errorf("failed to get modelServing of pod %s/%s: %v", resource.GetNamespace(), resource.GetName(), err)
		}
		return nil, "", "", ""
	}

	roleName, roleID := utils.GetRoleName(resource), utils.GetRoleID(resource)

	return ms, servingGroupName, roleName, roleID
}

func (c *ModelServingController) DeleteRole(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) (deleteErr error) {
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.GroupNameLabelKey: groupName,
		workloadv1alpha1.RoleLabelKey:      roleName,
		workloadv1alpha1.RoleIDKey:         roleID,
	})

	// If the role is already in the deletion process, no further processing will be done.

	err := c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, roleName, roleID, datastore.RoleDeleting)
	klog.V(4).Infof("Setting role %s/%s status to Deleting", ms.GetName(), roleID)
	if err != nil {
		klog.Errorf("failed to set role %s/%s status: %v", groupName, roleID, err)
		return err
	}

	// Emit event for role entering Deleting state.
	message := fmt.Sprintf("Role %s/%s in ServingGroup %s is now Deleting", roleName, roleID, groupName)
	c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "RoleDeleting", message)
	defer func() {
		if deleteErr != nil {
			c.enqueueModelServing(ms)
		}
	}()

	if deleteErr = c.prepareRoleCleanup(ctx, ms, groupName, roleName, roleID); deleteErr != nil {
		return deleteErr
	}
	podTargets, _ := c.podDeletions.LoadOrStore(auxiliaryCleanupKey{ms.UID, groupName, roleName, roleID}, &podDeletionTargets{})
	deleteErr = podTargets.(*podDeletionTargets).Delete(ctx, c.kubeClientSet, ms, selector)
	if deleteErr != nil {
		klog.Errorf("failed to delete pods of role %s/%s: %v", groupName, roleID, deleteErr)
		return deleteErr
	}
	if names, err := podTargets.(*podDeletionTargets).replacements(ctx, c.kubeClientSet, ms.Namespace, selector); err != nil {
		return err
	} else if len(names) > 0 {
		c.emitRoleStatusEvent(ms, corev1.EventTypeWarning, "DeletionIdentityChanged", fmt.Sprintf("Preserving replacement Pods %v; waiting for Role identity reconciliation", names))
		return nil
	}
	if deleteErr = c.runRoleDeletePlugins(ctx, ms, groupName, roleName, roleID); deleteErr != nil {
		return deleteErr
	}

	// Once the role's pods are fully deleted, remove the role from the store.
	// Note: This measure is taken to prevent the Role’s resources from being deleted before the current function execution has completed,
	// which would prevent them from being queued for re-coordination.
	if c.isRoleDeleted(ms, groupName, roleName, roleID) {
		klog.V(2).Infof("Role %s of ServingGroup %s has been deleted", roleID, groupName)
		c.store.DeleteRole(utils.GetNamespaceName(ms), groupName, roleName, roleID)
		c.roleCleanups.Delete(auxiliaryCleanupKey{ms.UID, groupName, roleName, roleID})
		c.podDeletions.Delete(auxiliaryCleanupKey{ms.UID, groupName, roleName, roleID})
		// Re-enqueue the ModelServing for reconciliation after the role has been deleted
		// so the controller can recreate any missing resources if needed.
		c.enqueueModelServing(ms)
	}
	return nil
}

// manageRollingUpdate updates outdated resources at the granularity selected by
// rolloutStrategy.type. ServingGroupRollingUpdate uses only the ModelServing-level
// maxUnavailable and partition. RoleRollingUpdate uses only each Role's
// maxUnavailable, partition and ServingGroup's partition.
//
// Main processing steps:
//  1. Identify the boundary for the currently active rollout partition.
//  2. Filter outdated groups (mismatched revision) that are allowed to be updated.
//  3. For ServingGroupRollingUpdate, enforce the ServingGroup-level maxUnavailable budget.
//  4. For RoleRollingUpdate, update outdated roles using each Role's maxUnavailable budget.
func (c *ModelServingController) manageRollingUpdateWithPolicy(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	revision string,
	rolloutPolicy *roleRolloutPolicy,
) error {
	ctx = withInPlaceRound(ctx)
	// Finish in-place updates first so their budget is released before selecting new units.
	if err := c.completeInPlaceUpdates(ctx, ms, revision); err != nil {
		return err
	}
	if roleCoordination(ms) != nil && ms.Spec.RolloutStrategy.UpdateMethod == workloadv1alpha1.InPlaceUpdateMethod {
		// Completion flips Pod labels and can release budget. Do not calculate
		// starts from the observation taken before those writes.
		ctx = withCoordinatedRound(ctx, ms)
		if err := c.refreshCoordinatedAvailabilityInRound(ctx, ms); err != nil {
			return err
		}
		var err error
		rolloutPolicy, err = c.resolveRoleRolloutPolicy(ctx, ms, revision)
		if err != nil {
			return err
		}
	}
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get ServingGroupList from store, err:%v", err)
	}

	partition, _, _ := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	// Separate outdated groups into two categories: not-running and running
	// We prioritize updating not-running outdated groups first
	var notRunningOutdatedGroups []datastore.ServingGroup
	var runningOutdatedGroups []datastore.ServingGroup
	var groupsAfterPartition []datastore.ServingGroup
	newServingGroupUnavailableCount := 0
	for _, group := range servingGroupList {
		if !partitionProtected(group.Name, partition) {
			groupsAfterPartition = append(groupsAfterPartition, group)
		} else if (ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate) &&
			group.Status != datastore.ServingGroupRunning && c.compareServingGroupTemplate(ctx, ms, group, revision) == templateUnknown {
			// Independent progress after a protected history error must not
			// treat the unverified group as spare availability outside partition.
			newServingGroupUnavailableCount++
		}
	}

	for _, sg := range groupsAfterPartition {
		comparison := c.compareServingGroupTemplate(ctx, ms, sg, revision)
		if sg.Status != datastore.ServingGroupRunning {
			if comparison != templateDifferent {
				// Unknown history cannot authorize deletion or provide spare
				// availability for deleting another Running group.
				newServingGroupUnavailableCount++
			} else {
				notRunningOutdatedGroups = append(notRunningOutdatedGroups, sg)
			}
		} else if comparison == templateDifferent {
			runningOutdatedGroups = append(runningOutdatedGroups, sg)
		}
	}

	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
		roleRollingGroups := make([]datastore.ServingGroup, 0, len(groupsAfterPartition))
		for _, servingGroup := range groupsAfterPartition {
			if servingGroup.Status != datastore.ServingGroupDeleting {
				roleRollingGroups = append(roleRollingGroups, servingGroup)
			}
		}
		updateCount, err := c.deleteOutdatedRoles(ctx, ms, roleRollingGroups, revision, rolloutPolicy)
		if err != nil {
			return err
		}
		if updateCount > 0 {
			klog.V(4).Infof("Started Role updates in %d ServingGroups for ModelServing %s", updateCount, ms.Name)
		}
		return nil
	}

	maxScaleDown := 0
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate {
		maxUnavailable, err := utils.GetMaxUnavailable(ms)
		if err != nil {
			return fmt.Errorf("failed to calculate maxUnavailable: %v", err)
		}

		// Calculate the minimum number of available ServingGroups required
		// Refer to https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/deployment/rolling.go
		// Check if we can scale down. We can scale down in the following 2 cases:
		// * Some old servingGroups are unhealthy, we could safely scale down those unhealthy servingGroups
		//   since that won't further increase unavailability.
		// * New servingGroup has scaled up and its replicas become ready, then we can scale down old servingGroups
		//   in a further step.
		minAvailable := modelServingReplicas(ms) - maxUnavailable
		// All Running ServingGroups, including temporary maxSurge capacity,
		// contribute to the availability budget. Unhealthy outdated groups can be
		// removed without reducing availability; an unavailable new-revision group
		// contributes nothing and may naturally reduce maxScaleDown to zero.
		maxScaleDown = len(servingGroupList) - minAvailable - newServingGroupUnavailableCount

		// TODO(hzxuzhonghu): reuse calMaxScaleDown
		if maxScaleDown <= 0 {
			klog.V(4).Infof("No ServingGroups can be updated for ModelServing %s/%s: maxScaleDown=%d",
				ms.Namespace, ms.Name, maxScaleDown)
			return nil
		}
	}

	allOutdatedGroups := append(runningOutdatedGroups, notRunningOutdatedGroups...)
	updateCount, err := c.deleteOutdatedServingGroups(ctx, ms, maxScaleDown, allOutdatedGroups, revision)
	if err != nil {
		return err
	}

	if updateCount > 0 {
		strategy := workloadv1alpha1.ServingGroupRollingUpdate
		if ms.Spec.RolloutStrategy != nil {
			strategy = ms.Spec.RolloutStrategy.Type
		}
		klog.V(4).Infof("Started updates in %d ServingGroups for ModelServing %s (strategy=%s)", updateCount, ms.Name, strategy)
	}
	return nil
}

// deleteOutdatedServingGroups deletes outdated ServingGroups
// for `ServingGroupRollingUpdate`.
func (c *ModelServingController) deleteOutdatedServingGroups(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	maxScaleDown int,
	groups []datastore.ServingGroup,
	revision string,
) (int, error) {
	updateCount := 0

	// Iterate from end to start to delete largest ordinals first.
	for i := len(groups) - 1; i >= 0 && updateCount < maxScaleDown; i-- {
		sg := groups[i]
		klog.V(2).Infof("ServingGroup %s will be terminated for update (status=%s)", sg.Name, sg.Status)
		if err := c.updateServingGroup(ctx, ms, sg, revision); err != nil {
			return updateCount, err
		}
		updateCount++
	}

	return updateCount, nil
}

// deleteOutdatedRoles deletes outdated Roles for `RoleRollingUpdate`.
func (c *ModelServingController) deleteOutdatedRoles(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	groups []datastore.ServingGroup,
	revision string,
	rolloutPolicy *roleRolloutPolicy,
) (int, error) {
	updateCount := 0

	// Iterate from end to start to delete largest ordinals first.
	for i := len(groups) - 1; i >= 0; i-- {
		sg := groups[i]
		rolesToDelete, hasOutdatedRoles, err := c.rolesToDeleteForRoleRollingUpdateInRound(ctx, ms, sg, rolloutPolicy.group(sg.Name))
		if err != nil {
			return updateCount, err
		}
		if !hasOutdatedRoles {
			c.updateServingGroupRevisionIfNoOutdatedRoles(ms, sg.Name, revision)
			continue
		}
		if len(rolesToDelete) == 0 {
			continue
		}
		for _, role := range rolesToDelete {
			actionCtx := ctx
			if roleCoordination(ms) != nil && ms.ResourceVersion != "" {
				// A previous admission or an asynchronous readiness/UID change
				// invalidates the planning snapshot. Recheck the entire budget
				// and dependency decision, not just the candidate entry Pod.
				actionCtx = withCoordinatedRound(ctx, ms)
				if err := c.refreshCoordinatedAvailabilityInRound(actionCtx, ms); err != nil {
					return updateCount, err
				}
				policy, err := c.resolveRoleRolloutPolicy(actionCtx, ms, revision)
				if err != nil {
					return updateCount, err
				}
				eligible, _, err := c.rolesToDeleteForRoleRollingUpdateInRound(actionCtx, ms, sg, policy.group(sg.Name))
				if err != nil {
					return updateCount, err
				}
				if !slices.Contains(eligible, role) {
					continue
				}
			}
			klog.V(2).Infof("Role %s/%s in ServingGroup %s will be terminated for update", role.roleName, role.roleID, sg.Name)
			if err := c.updateRole(actionCtx, ms, sg.Name, role.roleName, role.roleID, revision); err != nil {
				return updateCount, err
			}
		}
		updateCount++
	}

	return updateCount, nil
}

func (c *ModelServingController) updateServingGroupRevisionIfNoOutdatedRoles(ms *workloadv1alpha1.ModelServing, groupName, revision string) {
	if err := c.store.UpdateServingGroupRevision(utils.GetNamespaceName(ms), groupName, revision); err != nil {
		klog.Errorf("failed to update ServingGroup %s revision: %v", groupName, err)
		return
	}
	klog.V(2).Infof("Updated ServingGroup %s revision to latest: %s", groupName, revision)
}

type roleToDelete struct {
	roleName string
	roleID   string
}

func (c *ModelServingController) rolesToDeleteForRoleRollingUpdateInRound(ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	sg datastore.ServingGroup,
	groupPolicy *roleRolloutGroupPolicy,
) ([]roleToDelete, bool, error) {
	roleSpecByName := make(map[string]workloadv1alpha1.Role, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		roleSpecByName[role.Name] = role
	}

	allRoles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), sg.Name)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get roles for ServingGroup %s: %v", sg.Name, err)
	}

	var rolesToDelete []roleToDelete
	hasOutdatedRoles := groupPolicy != nil && groupPolicy.inProgress
	for _, roleSpec := range ms.Spec.Template.Roles {
		roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), sg.Name, roleSpec.Name)
		if err != nil {
			return nil, false, fmt.Errorf("failed to get roles for ServingGroup %s, role %s: %v", sg.Name, roleSpec.Name, err)
		}

		if roleCoordination(ms) != nil {
			marked, err := c.surgePodsInRound(ctx, ms, surgeRole, sg.Name, roleSpec.Name)
			if err != nil {
				return nil, false, err
			}
			if len(roleList) != roleReplicas(roleSpec) || markedSurgeNames(marked, surgeRole, roleReplicas(roleSpec)).Len() > 0 {
				hasOutdatedRoles = true
			}
		}
		for _, instance := range roleList {
			if c.compareRoleTemplate(ctx, ms, sg, roleSpec.Name, instance) == templateUnknown {
				hasOutdatedRoles = true
			}
		}
		outdatedRoles, newUnavailable := c.outdatedRolesInRound(ctx, ms, sg, roleSpec, roleList)
		partition, _, partitionErr := c.getPartition(rolePartition(ms, roleSpec), roleReplicas(roleSpec))
		if partitionErr != nil {
			return nil, false, fmt.Errorf("failed to parse partition for role %s: %v", roleSpec.Name, partitionErr)
		}
		protected := sets.New[string]()
		for _, role := range roleList {
			if partitionProtected(role.Name, partition) {
				protected.Insert(role.Name)
			}
		}
		if len(protected) > 0 && len(outdatedRoles) > 0 {
			filtered := outdatedRoles[:0]
			for _, r := range outdatedRoles {
				if protected.Has(r.Name) {
					continue
				}
				filtered = append(filtered, r)
			}
			outdatedRoles = filtered
		}

		// Keep the existing ServingGroup revision semantics: protected outdated
		// replicas still mean this ServingGroup has not fully reached the revision,
		// even though they are not rolling candidates.
		if len(outdatedRoles) == 0 {
			if len(protected) > 0 {
				for _, role := range roleList {
					if !protected.Has(role.Name) {
						continue
					}
					if role.Status == datastore.RoleDeleting {
						continue
					}
					if c.compareRoleTemplate(ctx, ms, sg, roleSpec.Name, role) == templateDifferent {
						hasOutdatedRoles = true
						break
					}
				}
			}
		}

		if len(outdatedRoles) > 0 {
			hasOutdatedRoles = true
		}

		if len(outdatedRoles) == 0 {
			continue
		}
		budget, budgetErr := roleRolloutBudget(roleSpec, roleList, newUnavailable)
		if budgetErr != nil {
			klog.ErrorS(budgetErr, "Invalid Role rollout budget", "role", roleSpec.Name, "group", sg.Name)
			continue
		}
		localCandidates, err := c.selectRoleRolloutCandidates(ctx, ms, sg, roleSpec, roleList, outdatedRoles, budget, groupPolicy)
		if err != nil {
			return nil, false, err
		}
		rolesToDelete = append(rolesToDelete, localCandidates...)
	}

	// handle the case when there are roles whose roleSpec has been deleted in the new revision. Those roles should be deleted directly since they are all outdated.
	for roleName, roles := range allRoles {
		if _, ok := roleSpecByName[roleName]; ok {
			continue
		}
		for roleID, role := range roles {
			if role.Status == datastore.RoleDeleting {
				continue
			}
			hasOutdatedRoles = true
			rolesToDelete = append(rolesToDelete, roleToDelete{roleName: roleName, roleID: roleID})
		}
	}

	return rolesToDelete, hasOutdatedRoles, nil
}

func (c *ModelServingController) outdatedRolesInRound(ctx context.Context, ms *workloadv1alpha1.ModelServing, sg datastore.ServingGroup, roleSpec workloadv1alpha1.Role, roleList []datastore.Role) ([]datastore.Role, int) {
	outdatedRoles := make([]datastore.Role, 0, len(roleList))
	// record the number of roles that is in rollingupdate but not ready yet.
	newUnavailable := 0
	for i, role := range roleList {
		if role.Status == datastore.RoleDeleting || c.rolloutDeletionPending(ms, sg.Name, roleSpec.Name, role.Name) {
			roleList[i].Status = datastore.RoleDeleting
			newUnavailable++
			continue
		}
		if c.coordinatedInPlaceStarted(ctx, ms, sg.Name, roleSpec.Name, role.Name) {
			newUnavailable++
			continue
		}
		comparison := c.compareRoleTemplate(ctx, ms, sg, roleSpec.Name, role)
		if comparison == templateUnknown {
			if role.Status != datastore.RoleRunning {
				newUnavailable++
			}
			continue
		}
		if comparison == templateDifferent {
			outdatedRoles = append(outdatedRoles, role)
		} else if role.Status != datastore.RoleRunning {
			newUnavailable++
		}
	}

	slices.SortFunc(outdatedRoles, func(a, b datastore.Role) int {
		if roleCoordination(ms) == nil && a.Status != b.Status {
			if a.Status != datastore.RoleRunning {
				return -1
			}
			return 1
		}
		_, aOrdinal := utils.GetParentNameAndOrdinal(a.Name)
		_, bOrdinal := utils.GetParentNameAndOrdinal(b.Name)
		return cmp.Compare(bOrdinal, aOrdinal)
	})
	return outdatedRoles, newUnavailable
}

func (c *ModelServingController) handleReadyPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, newPod *corev1.Pod) error {
	workload := ms
	var revisionErr error
	// Hooks for old Pods must use their historical plugin chain, not the new
	// desired chain during a partitioned rollout.
	if len(ms.Spec.Plugins) > 0 || utils.ObjectRevision(newPod) != "" {
		var err error
		workload, err = c.revisionHistory(context.Background(), ms).workload(context.Background(), utils.ObjectRevision(newPod))
		if err != nil {
			revisionErr = fmt.Errorf("resolve ready Pod revision: %w", err)
			c.enqueueModelServingAfter(ms, enqueueAfter)
		}
	}
	if revisionErr == nil {
		chain, err := c.buildPluginChain(workload)
		if err != nil {
			return fmt.Errorf("build plugin chain: %w", err)
		}
		if chain != nil {
			if err := chain.OnPodReady(context.Background(), &plugins.HookRequest{
				ModelServing:  workload,
				ServingGroup:  servingGroupName,
				RoleName:      utils.GetRoleName(newPod),
				RoleID:        utils.GetRoleID(newPod),
				IsEntry:       newPod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry,
				Pod:           newPod,
				KubeClient:    c.kubeClientSet,
				ServiceLister: c.servicesLister,
			}); err != nil {
				return err
			}
		}
	}

	// Unknown history defers hooks and template changes, not observation of a
	// healthy Pod. In particular, a restart must not forget its Role/revision
	// and mistake an existing ServingGroup for missing capacity.
	roleName := utils.GetRoleName(newPod)
	roleID := utils.GetRoleID(newPod)
	roleTemplateHash := c.resolveRoleTemplateHash(ms, roleName, newPod)
	c.store.AddRunningPodToServingGroup(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	}, servingGroupName, newPod.Name, utils.ObjectRevision(newPod), roleTemplateHash, roleName, roleID)

	// Check and update role status to Running when all pods in the role are ready
	roleReady, err := c.checkRoleReady(ms, servingGroupName, roleName, roleID)
	if err != nil {
		klog.Warningf("failed to check role %s/%s readiness, skipping role status update: %v", roleName, roleID, err)
	} else if roleReady {
		changed, err := c.markRoleStatus(ms, servingGroupName, roleName, roleID, datastore.RoleRunning)
		if err != nil {
			klog.Warningf("failed to update role %s/%s status to Running: %v", roleName, roleID, err)
		} else if changed {
			// A Role that becomes Running can release coordination or RoleRollingUpdate
			// budget before its ServingGroup is Running, for example a Role maxSurge
			// replica. Reconcile so that budget is spent.
			c.enqueueModelServing(ms)
		}
	}

	ready, err := c.checkServingGroupReady(ms, servingGroupName)
	if err != nil {
		return errors.Join(revisionErr, fmt.Errorf("failed to check ServingGroup status: %w", err))
	}
	if ready {
		// All pods in the ServingGroup are running, so the ServingGroup status also needs to be set to running
		err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupRunning)
		klog.V(4).Infof("ServingGroup: %s/%s status updated to Running", ms.GetName(), servingGroupName)
		if err != nil {
			return fmt.Errorf("failed to set ServingGroup %s status: %v", servingGroupName, err)
		}
		klog.V(2).Infof("Update ServingGroup %s status to Running", servingGroupName)
		c.enqueueModelServing(ms)
	} else {
		klog.V(4).Infof("ServingGroup %s still creating", servingGroupName)
	}
	return revisionErr
}

// recoveryDisabled reports whether the controller must never delete a faulty Pod:
// recoveryPolicy is None or restartGracePeriodSeconds is -1. A Pod deleted by
// others is still handled by handleDeletedPod.
func recoveryDisabled(ms *workloadv1alpha1.ModelServing) bool {
	grace := ms.Spec.Template.RestartGracePeriodSeconds
	return ms.Spec.RecoveryPolicy == workloadv1alpha1.NoneRestartPolicy || (grace != nil && *grace == -1)
}

func (c *ModelServingController) handleErrorPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, errPod *corev1.Pod) error {
	// Recovery disabled: leave the pod to the kubelet (do not delete it), but mark it unavailable.
	if recoveryDisabled(ms) {
		if err := c.markPodUnavailable(ms, servingGroupName, errPod); err != nil {
			klog.Warningf("mark pod %s unavailable: %v", errPod.Name, err)
		}
		c.enqueueModelServing(ms)
		return nil
	}
	// pod is already in the grace period and does not need to be processed for the time being.
	key := getPodGracePeriodKey(errPod)
	now := time.Now()
	_, loaded := c.graceMap.LoadOrStore(key, now)
	if loaded {
		klog.V(4).Infof("Pod %v already in grace period", key)
		return nil
	}
	if err := c.markPodUnavailable(ms, servingGroupName, errPod); err != nil {
		c.graceMap.Delete(key)
		return err
	}
	// Wait for the grace period before processing
	go c.handlePodAfterGraceTime(ms, errPod)
	// ServingGroup status may change, needs reconcile
	c.enqueueModelServing(ms)
	return nil
}

// markPodUnavailable removes the pod from the running set and transitions its
// role/serving group out of Running so AvailableReplicas stops counting it. It
// does not delete the pod.
func (c *ModelServingController) markPodUnavailable(ms *workloadv1alpha1.ModelServing, servingGroupName string, errPod *corev1.Pod) error {
	c.store.DeleteRunningPodFromServingGroup(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	}, servingGroupName, errPod.Name)

	roleName := utils.GetRoleName(errPod)
	roleID := utils.GetRoleID(errPod)
	// Update role status back to Creating when pod fails
	if _, err := c.markRoleStatus(ms, servingGroupName, roleName, roleID, datastore.RoleCreating); err != nil {
		klog.Warningf("failed to update role %s/%s status to Creating: %v", roleName, roleID, err)
	}

	// If the ServingGroup status is already running, the status needs to be updated
	if groupStatus := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName); groupStatus == datastore.ServingGroupRunning {
		if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupCreating); err != nil {
			return fmt.Errorf("update ServingGroup status failed, err:%v", err)
		}
		klog.V(2).Infof("update ServingGroup %s to processing when pod fails", servingGroupName)
	}
	return nil
}

func (c *ModelServingController) handlePodAfterGraceTime(ms *workloadv1alpha1.ModelServing, errPod *corev1.Pod) {
	if ms.Spec.Template.RestartGracePeriodSeconds != nil && *ms.Spec.Template.RestartGracePeriodSeconds > 0 {
		// Wait for the grace period before making a decision
		time.Sleep(time.Duration(*ms.Spec.Template.RestartGracePeriodSeconds) * time.Second)
		klog.V(4).Infof("%s after grace time", errPod.Name)
		defer c.graceMap.Delete(getPodGracePeriodKey(errPod))

		newPod, err := c.podsLister.Pods(ms.Namespace).Get(errPod.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("pod %s has been deleted after grace time", errPod.Name)
			} else {
				klog.Errorf("cannot get pod %s after grace time, err: %v", errPod.Name, err)
			}
			return
		}
		if newPod.UID != errPod.UID {
			klog.V(4).Infof("pod %s has been replaced after grace time", errPod.Name)
			return
		}

		if !utils.IsPodRunningAndReady(newPod) || inplace.Updating(newPod) {
			// pod has not recovered after the grace period (an unfinished in-place update counts as not recovered), needs to be rebuilt
			// After this pod has been deleted, we will rebuild the ServingGroup in deletePod function
			err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(context.TODO(), newPod.Name, *metav1.NewPreconditionDeleteOptions(string(errPod.UID)))
			if err != nil {
				klog.Errorf("cannot delete pod %s after grace time, err: %v", newPod.Name, err)
				return
			}
			klog.V(2).Infof("%s been deleted after grace time", errPod.Name)
		}
	} else {
		// grace period is not set or the grace period is 0, the deletion will be executed immediately.
		defer c.graceMap.Delete(getPodGracePeriodKey(errPod))

		err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(context.TODO(), errPod.Name, *metav1.NewPreconditionDeleteOptions(string(errPod.UID)))
		if err != nil {
			klog.Errorf("cannot delete pod %s when it error, err: %v", errPod.Name, err)
			return
		}
		klog.V(2).Infof("%s been deleted without grace time", errPod.Name)
	}
}

func (c *ModelServingController) handleDeletedPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) error {
	// pod is deleted due to failure or other reasons and needs to be rebuilt according to the RecoveryPolicy
	switch ms.Spec.RecoveryPolicy {
	case workloadv1alpha1.ServingGroupRecreate:
		// Rebuild the entire ServingGroup directly
		if err := c.deleteServingGroup(context.TODO(), ms, servingGroupName); err != nil {
			klog.Errorf("failed to delete ServingGroup %s: %v", servingGroupName, err)
		}
	case workloadv1alpha1.RoleRecreate:
		// If Rolling update in RoleRecreate mode, requires re-entering the queue during the pod delete event.
		if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName) == datastore.ServingGroupDeleting {
			if err := c.deleteServingGroup(context.TODO(), ms, servingGroupName); err != nil {
				klog.Errorf("failed to delete ServingGroup %s: %v", servingGroupName, err)
			}
			return nil
		} else if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName) == datastore.ServingGroupRunning {
			// If the ServingGroup status is running when the pod fails, we need to set it to creating
			err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupCreating)
			klog.V(4).Infof("Setting ServingGroup %s/%s status to Creating when pod deleted for recreating", ms.GetName(), servingGroupName)
			if err != nil {
				return fmt.Errorf("failed to set ServingGroup %s status: %v", servingGroupName, err)
			}
		}
		if err := c.DeleteRole(context.Background(), ms, servingGroupName, utils.GetRoleName(pod), utils.GetRoleID(pod)); err != nil {
			return err
		}
	case workloadv1alpha1.NoneRestartPolicy:
		// None: re-enqueue to refill the single missing pod (not the whole role/group).
		if err := c.markPodUnavailable(ms, servingGroupName, pod); err != nil {
			klog.Warningf("mark pod %s unavailable: %v", pod.Name, err)
		}
		c.enqueueModelServing(ms)
	}
	return nil
}

func (c *ModelServingController) checkServingGroupReady(ms *workloadv1alpha1.ModelServing, servingGroupName string) (bool, error) {
	return c.checkServingGroupReadyWithContext(c.withRevisionHistory(context.Background(), ms), ms, servingGroupName)
}

func (c *ModelServingController) checkServingGroupReadyWithContext(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) (bool, error) {
	klog.V(4).Infof("checkServingGroupReady: modelServing=%s/%s, servingGroup=%s", ms.Namespace, ms.Name, servingGroupName)
	roles, err := c.rolesForServingGroupReadinessWithContext(ctx, ms, servingGroupName)
	if err != nil {
		return false, err
	}
	for _, role := range roles {
		roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), servingGroupName, role.Name)
		if err != nil {
			return false, err
		}
		if len(roleList) != int(*role.Replicas) {
			klog.V(4).Infof("checkServingGroupReady: role %s in group %s not ready: replica count mismatch (%d/%d)",
				role.Name, servingGroupName, len(roleList), int(*role.Replicas))
			return false, nil
		}
		for _, r := range roleList {
			if r.Status != datastore.RoleRunning {
				klog.V(4).Infof("checkServingGroupReady: role %s/%s in group %s not ready: status=%s",
					role.Name, r.Name, servingGroupName, r.Status)
				return false, nil
			}
			ready, err := c.checkRoleReadyInRound(ctx, ms, servingGroupName, role.Name, r.Name)
			if err != nil || !ready {
				return false, err
			}
		}
	}
	klog.V(4).Infof("checkServingGroupReady: servingGroup %s is ready", servingGroupName)
	return true, nil
}

func (c *ModelServingController) checkRoleReadyInRound(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) (bool, error) {
	pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID))
	if err != nil {
		return false, err
	}
	if roleCoordination(ms) != nil && ms.ResourceVersion != "" {
		pods, err = c.coordinatedPods(ctx, ms, labels.Set{workloadv1alpha1.GroupNameLabelKey: groupName, workloadv1alpha1.RoleLabelKey: roleName, workloadv1alpha1.RoleIDKey: roleID}.AsSelector())
		if err != nil {
			return false, err
		}
	}
	instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, roleName)
	if err != nil {
		return false, err
	}
	for _, instance := range instances {
		if instance.Name != roleID {
			continue
		}
		if instance.Status == datastore.RoleDeleting {
			return false, nil
		}
		if roleCoordination(ms) != nil && ms.ResourceVersion != "" {
			live, pod, err := c.coordinatedLiveRole(ctx, ms, groupName, roleName, instance)
			if err != nil || pod == nil {
				return false, err
			}
			instance = live
		}
		role, revision, _, err := c.appliedRoleTemplate(ctx, ms, groupName, roleName, instance)
		if err != nil {
			return false, err
		}
		return rolePodsComplete(ms, groupName, roleID, role, revision, pods, true), nil
	}
	return false, nil
}

// Role mode uses current instance counts; each instance's worker layout is
// resolved separately. SG mode uses its applied snapshot with current counts.
func (c *ModelServingController) rolesForServingGroupReadiness(ms *workloadv1alpha1.ModelServing, groupName string) ([]workloadv1alpha1.Role, error) {
	return c.rolesForServingGroupReadinessWithContext(c.withRevisionHistory(context.Background(), ms), ms, groupName)
}

func (c *ModelServingController) rolesForServingGroupReadinessWithContext(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string) ([]workloadv1alpha1.Role, error) {
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
		return ms.Spec.Template.Roles, nil
	}
	revision, _ := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), groupName)
	return c.revisionHistory(ctx, ms).roles(ctx, revision)
}

func (c *ModelServingController) isServingGroupOutdated(group datastore.ServingGroup, namespace, newRevision string) bool {
	// Find the pods corresponding to ServingGroup
	groupNameValue := fmt.Sprintf("%s/%s", namespace, group.Name)
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("cannot list pod when check ServingGroup %s/%s updated: %v", namespace, group.Name, err)
		return false
	}
	// Check all pods match the newHash
	for _, pod := range pods {
		if utils.ObjectRevision(pod) != newRevision {
			return true
		}
	}
	return false
}

// getModelServingByChildResource gets the ModelServing and group name for any resource that has the appropriate labels
func (c *ModelServingController) getModelServingByChildResource(resource metav1.Object) (*workloadv1alpha1.ModelServing, string, error) {
	modelServingName, servingGroupName, ok := utils.GetModelServingAndGroupByLabel(resource.GetLabels())
	if !ok {
		return nil, "", fmt.Errorf("cannot get modelServing name and ServingGroup name from resource %s/%s", resource.GetNamespace(), resource.GetName())
	}
	ms, err := c.modelServingLister.ModelServings(resource.GetNamespace()).Get(modelServingName)
	if err != nil {
		return nil, "", err
	}
	return ms, servingGroupName, nil
}

// shouldSkipHandling checks if an object should be skipped because it is not
// owned by the ModelServing or does not carry a controller-generated identity.
func (c *ModelServingController) shouldSkipHandling(ms *workloadv1alpha1.ModelServing, servingGroupName string, obj metav1.Object) bool {
	if !utils.IsOwnedByModelServingWithUID(obj, ms.UID) {
		// If the pod is not owned by the ModelServing, we do not need to handle it.
		klog.V(4).Infof("object %s/%s maybe left from previous same named ModelServing %s/%s, skip handling",
			obj.GetNamespace(), obj.GetName(), ms.Namespace, ms.Name)
		return true
	}
	if !hasGeneratedIdentity(ms, servingGroupName, obj.GetLabels()) {
		klog.Warningf("object %s/%s has a ServingGroup or Role identity not generated for ModelServing %s/%s, skip handling",
			obj.GetNamespace(), obj.GetName(), ms.Namespace, ms.Name)
		return true
	}
	return false
}

// hasGeneratedIdentity reports whether the ServingGroup and optional Role ID
// have the "<parent>-<ordinal>" form generated by the controller.
func hasGeneratedIdentity(ms *workloadv1alpha1.ModelServing, servingGroupName string, labels map[string]string) bool {
	if parent, ordinal := utils.GetParentNameAndOrdinal(servingGroupName); parent != ms.Name || ordinal < 0 {
		return false
	}
	roleID, ok := labels[workloadv1alpha1.RoleIDKey]
	if !ok {
		return true
	}
	parent, ordinal := utils.GetParentNameAndOrdinal(roleID)
	return parent == labels[workloadv1alpha1.RoleLabelKey] && ordinal >= 0
}

func getMetaObject(obj interface{}) metav1.Object {
	if metaObj, ok := obj.(metav1.Object); ok {
		return metaObj
	}

	// Handle tombstone object
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if metaObj, ok := tombstone.Obj.(metav1.Object); ok {
			return metaObj
		}
	}

	return nil
}

func isOwnedByModelServing(metaObj metav1.Object) bool {
	for _, ownerRef := range metaObj.GetOwnerReferences() {
		if ownerRef.APIVersion == workloadv1alpha1.SchemeGroupVersion.String() && ownerRef.Kind == "ModelServing" {
			return true
		}
	}
	return false
}

// handleDeletionInProgress checks and handles deletion states for ServingGroup or Role.
// Returns true if the resource deletion is already in progress and the caller should stop further handling.
func (c *ModelServingController) handleDeletionInProgress(ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) bool {
	// Informer callbacks must not discard the identities needed by cleanup. Only
	// the workqueue finalizes deletion after all cleanup requests were accepted.
	if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName) == datastore.ServingGroupDeleting ||
		(roleName != "" && roleID != "" && c.store.GetRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID) == datastore.RoleDeleting) {
		c.enqueueModelServing(ms)
		return true
	}
	return false
}

func (c *ModelServingController) isServingGroupDeleted(ms *workloadv1alpha1.ModelServing, servingGroupName string) bool {
	status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName)
	if status != datastore.ServingGroupDeleting {
		// It will be Determined whether all resource have been deleted only when the group status is deleting.
		return false
	}
	// check whether the ServingGroup deletion has been completed
	groupNameValue := fmt.Sprintf("%s/%s", ms.Namespace, servingGroupName)
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("failed to get pods for ServingGroup %s of ModelServing %s/%s: %v", servingGroupName, ms.Namespace, ms.Name, err)
		return false
	}
	pgs := []*schedulingv1beta1.PodGroup{}
	if c.podGroupManager.HasPodGroupCRD() {
		pgs, err = c.getPodGroupsByIndex(GroupNameKey, groupNameValue)
		if err != nil {
			klog.Errorf("failed to get podGroups for ServingGroup %s of ModelServing %s/%s: %v", servingGroupName, ms.Namespace, ms.Name, err)
			return false
		}
	}
	return len(pgs) == 0 && len(pods) == 0
}

func (c *ModelServingController) isRoleDeleted(ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) bool {
	if c.store.GetRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID) != datastore.RoleDeleting {
		// It will be Determined whether all resource have been deleted only when the role status is deleting.
		return false
	}
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, servingGroupName, roleName, roleID)
	// check whether the role deletion has been completed
	pods, err := c.getPodsByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		klog.Errorf("failed to get pods for role %s/%s in ServingGroup %s of ModelServing %s/%s: %v", roleName, roleID, servingGroupName, ms.Namespace, ms.Name, err)
		return false
	}
	return len(pods) == 0
}

// getPodsByIndex filter pods using the informer indexer.
func (c *ModelServingController) getPodsByIndex(indexName, indexValue string) ([]*corev1.Pod, error) {
	indexer := c.podsInformer.GetIndexer()
	if _, exists := indexer.GetIndexers()[indexName]; !exists {
		return nil, fmt.Errorf("pod indexer %s not found", indexName)
	}
	objs, err := indexer.ByIndex(indexName, indexValue)
	if err != nil {
		return nil, err
	}

	var pods []*corev1.Pod
	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("unexpected object type in pod indexer: %T", obj)
			continue
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// TODO: move to podgroup manager
func (c *ModelServingController) getPodGroupsByIndex(indexName, indexValue string) ([]*schedulingv1beta1.PodGroup, error) {
	if c.podGroupManager == nil || !c.podGroupManager.HasPodGroupCRD() {
		return nil, nil
	}

	podGroupInformer := c.podGroupManager.GetPodGroupInformer()
	if podGroupInformer == nil {
		return nil, fmt.Errorf("podGroup informer is not initialized")
	}
	indexer := podGroupInformer.GetIndexer()
	if indexer == nil {
		return nil, fmt.Errorf("podGroup informer indexer is not initialized")
	}
	if _, exists := indexer.GetIndexers()[indexName]; !exists {
		return nil, fmt.Errorf("podGroup indexer %s not found", indexName)
	}
	objs, err := indexer.ByIndex(indexName, indexValue)
	if err != nil {
		return nil, err
	}

	var podGroups []*schedulingv1beta1.PodGroup
	for _, obj := range objs {
		podGroup, ok := obj.(*schedulingv1beta1.PodGroup)
		if !ok {
			klog.Errorf("unexpected object type in podGroup indexer: %T", obj)
			continue
		}
		podGroups = append(podGroups, podGroup)
	}
	return podGroups, nil
}

// UpdateModelServingStatus update replicas in modelServing status.
func (c *ModelServingController) UpdateModelServingStatus(ms *workloadv1alpha1.ModelServing, revision string) error {
	rolloutPolicy, err := c.resolveRoleRolloutPolicy(context.Background(), ms, revision)
	if err != nil {
		return err
	}
	return c.updateModelServingStatus(ms, revision, rolloutPolicy)
}

func (c *ModelServingController) updateModelServingStatusInRound(ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	revision string,
	rolloutPolicy *roleRolloutPolicy,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get latest modelserving from informer store
		latestMS, getErr := c.modelServingLister.ModelServings(ms.Namespace).Get(ms.Name)
		if getErr != nil {
			return getErr
		}

		// Calculate status based on latestMS
		groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(latestMS))
		if err != nil || (len(groups) == 0 && roleCoordination(latestMS) != nil && modelServingReplicas(latestMS) == 0) {
			// If no groups exist, handle gracefully by setting revisions to the new revision
			if err == nil || errors.Is(err, datastore.ErrServingGroupNotFound) {
				copy := latestMS.DeepCopy()
				selectorSet := labels.Set{
					workloadv1alpha1.ModelServingNameLabelKey: latestMS.Name,
					workloadv1alpha1.EntryLabelKey:            utils.Entry,
				}
				if len(latestMS.Spec.Template.Roles) > 0 {
					roleName := latestMS.Spec.Template.Roles[0].Name
					selectorSet[workloadv1alpha1.RoleLabelKey] = roleName
					selectorSet[workloadv1alpha1.RoleIDKey] = utils.GenerateRoleID(roleName, 0)
				}
				selector := selectorSet.String()
				conditionChanged, _ := rolloutPolicy.setCondition(copy)
				currentRevision := revision
				if roleCoordination(latestMS) != nil && modelServingReplicas(latestMS) == 0 {
					// Removing all groups supplies no evidence that an unfinished
					// target template completed. Retain the last completed baseline.
					currentRevision = copy.Status.CurrentRevision
				}
				needsUpdate := copy.Status.CurrentRevision != currentRevision || copy.Status.UpdateRevision != revision || copy.Status.LabelSelector != selector || conditionChanged
				if roleCoordination(latestMS) != nil && modelServingReplicas(latestMS) == 0 {
					needsUpdate = needsUpdate || copy.Status.Replicas != 0 || copy.Status.AvailableReplicas != 0 || copy.Status.CurrentReplicas != 0 || copy.Status.UpdatedReplicas != 0 || copy.Status.ObservedGeneration != latestMS.Generation
					copy.Status.Replicas, copy.Status.AvailableReplicas = 0, 0
					copy.Status.CurrentReplicas, copy.Status.UpdatedReplicas = 0, 0
					copy.Status.ObservedGeneration = latestMS.Generation
				}
				if c.revisionHistory(ctx, ms).copyCollisionCount(copy) {
					needsUpdate = true
				}
				if needsUpdate {
					copy.Status.CurrentRevision = currentRevision
					copy.Status.UpdateRevision = revision
					copy.Status.LabelSelector = selector
					_, updateErr := c.modelServingClient.WorkloadV1alpha1().ModelServings(copy.GetNamespace()).UpdateStatus(context.TODO(), copy, metav1.UpdateOptions{})
					return updateErr
				}
				return nil
			}
			return err
		}

		replicas := modelServingReplicas(latestMS)
		partition, _, partitionErr := c.getPartition(modelServingPartition(latestMS), replicas)
		if partitionErr != nil {
			return fmt.Errorf("failed to resolve rollout partition: %v", partitionErr)
		}
		available, updated := 0, 0
		progressingGroups, updatedGroups, currentGroups := []int{}, []int{}, []int{}
		// Track revision counts to determine the most common non-updated revision (CurrentRevision)
		revisionCount := make(map[string]int)
		referencedRevisions := make([]string, 0, len(groups))
		roleCountsComplete := true
		rolloutActive := c.hasUpdateableOutdatedServingGroup(ctx, latestMS, groups, revision, partition)
		for index := range groups {
			group := groups[index]
			_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
			if group.Revision != "" {
				referencedRevisions = append(referencedRevisions, group.Revision)
			}
			// Role mode can advance the group while unchanged Roles still use older history.
			roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(latestMS), group.Name)
			if err != nil {
				return fmt.Errorf("collect Role revision references for %s: %w", group.Name, err)
			}
			for _, desired := range latestMS.Spec.Template.Roles {
				if len(roles[desired.Name]) != roleReplicas(desired) {
					roleCountsComplete = false
				}
			}
			for _, instances := range roles {
				for _, role := range instances {
					if role != nil && role.Revision != "" {
						referencedRevisions = append(referencedRevisions, role.Revision)
					}
				}
			}
			if group.Status == datastore.ServingGroupDeleting {
				// Scaling -> Running or
				// Creating -> Running
				// No Deleting -> Running.
				// So directly add deleting groups to progressingGroups
				progressingGroups = append(progressingGroups, ordinal)
				continue
			}

			if group.Status == datastore.ServingGroupRunning && roleCoordination(latestMS) == nil {
				available = available + 1
			} else if ok, err := c.checkServingGroupReadyWithContext(ctx, latestMS, group.Name); ok && err == nil {
				// some scenarios, pod events may not trigger group status updates, such as role scaling down.
				err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(latestMS), group.Name, datastore.ServingGroupRunning)
				if err != nil {
					return fmt.Errorf("failed to set servingGroup %s status: %v", group.Name, err)
				}
				available = available + 1
				klog.V(2).Infof("Update servingGroup %s status to Running", group.Name)
			} else {
				if roleCoordination(latestMS) != nil && group.Status == datastore.ServingGroupRunning {
					if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(latestMS), group.Name, datastore.ServingGroupCreating); err != nil {
						return err
					}
				}
				progressingGroups = append(progressingGroups, ordinal)
			}

			if c.compareServingGroupTemplate(ctx, latestMS, group, revision) == templateEquivalent {
				updated = updated + 1
				updatedGroups = append(updatedGroups, ordinal)
			} else {
				currentGroups = append(currentGroups, ordinal)
				// Count revisions for non-updated groups to find the most common one
				revisionCount[group.Revision]++
			}
		}
		progressActive := len(progressingGroups) > 0 || len(groups) != replicas || available != replicas

		copy := latestMS.DeepCopy()
		shouldUpdate := utils.SetConditionWithRolloutAndProgressState(
			copy, progressingGroups, updatedGroups, currentGroups, rolloutActive, progressActive,
		)
		if c.revisionHistory(ctx, ms).copyCollisionCount(copy) {
			shouldUpdate = true
		}
		coordinationConditionChanged, coordinationCondition := rolloutPolicy.setCondition(copy)
		shouldUpdate = shouldUpdate || coordinationConditionChanged

		// A replacement gap must not discard the baseline used by protected slots.
		updateRevision := revision
		var currentRevision string
		rolloutComplete := roleCountsComplete && updated == replicas && available == replicas && len(groups) == replicas

		if copy.Status.CurrentRevision != "" {
			currentRevision = copy.Status.CurrentRevision
			if rolloutComplete {
				currentRevision = updateRevision
			}
		}

		// If CurrentRevision is absent, compute it from current groups.
		if currentRevision == "" {
			if rolloutComplete || len(revisionCount) == 0 {
				// All groups are updated or no groups exist
				currentRevision = updateRevision
			} else {
				// Find the revision with the highest count among non-updated groups
				maxCount := 0
				for rev, count := range revisionCount {
					if count > maxCount {
						maxCount = count
						currentRevision = rev
					}
				}
				// If no current revision found (shouldn't happen), fallback to updateRevision
				if currentRevision == "" {
					currentRevision = updateRevision
				}
			}
		}

		current := 0
		for _, group := range groups {
			if (currentRevision == revision && c.compareServingGroupTemplate(ctx, latestMS, group, revision) == templateEquivalent) ||
				(currentRevision != revision && group.Revision == currentRevision) {
				current++
			}
		}
		if copy.Status.Replicas != int32(len(groups)) || copy.Status.AvailableReplicas != int32(available) || copy.Status.UpdatedReplicas != int32(updated) || copy.Status.CurrentReplicas != int32(current) {
			shouldUpdate = true
			copy.Status.Replicas = int32(len(groups))
			copy.Status.AvailableReplicas = int32(available)
			copy.Status.UpdatedReplicas = int32(updated)
			copy.Status.CurrentReplicas = int32(current)
		}

		revisionUpdated := false
		if copy.Status.CurrentRevision != currentRevision || copy.Status.UpdateRevision != updateRevision {
			shouldUpdate = true
			revisionUpdated = true
			copy.Status.CurrentRevision = currentRevision
			copy.Status.UpdateRevision = updateRevision
		}

		if copy.Spec.RolloutStrategy == nil || copy.Spec.RolloutStrategy.RollingUpdateConfiguration == nil || copy.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition == nil {
			// if not set spec.RolloutStrategy.RollingUpdateConfiguration.Partition,
			// should set currentReplicas = updatedReplicas when rolling update is over.
			if copy.Status.UpdatedReplicas == *copy.Spec.Replicas &&
				copy.Status.AvailableReplicas == *copy.Spec.Replicas &&
				copy.Status.Replicas == *copy.Spec.Replicas {
				shouldUpdate = true
				copy.Status.CurrentReplicas = copy.Status.UpdatedReplicas
			}
		}

		if copy.Status.ObservedGeneration != latestMS.Generation {
			shouldUpdate = true
			copy.Status.ObservedGeneration = latestMS.Generation
		}

		// Set labelSelector so the scale subresource can report it to HPA.
		// spec.replicas counts ServingGroups, not pods, so the selector must
		// match exactly one pod per group — otherwise HPA/KEDA sees a pod count
		// that is a multiple of the group count and scales incorrectly.
		// Pin to the entry pod of the 0th instance of the first role: there is
		// exactly one such pod per group, regardless of role.Replicas.
		selectorSet := labels.Set{
			workloadv1alpha1.ModelServingNameLabelKey: latestMS.Name,
			workloadv1alpha1.EntryLabelKey:            utils.Entry,
		}
		if len(latestMS.Spec.Template.Roles) > 0 {
			roleName := latestMS.Spec.Template.Roles[0].Name
			selectorSet[workloadv1alpha1.RoleLabelKey] = roleName
			selectorSet[workloadv1alpha1.RoleIDKey] = utils.GenerateRoleID(roleName, 0)
		}
		selector := selectorSet.String()
		if copy.Status.LabelSelector != selector {
			shouldUpdate = true
			copy.Status.LabelSelector = selector
		}

		if shouldUpdate {
			_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(copy.GetNamespace()).UpdateStatus(context.TODO(), copy, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			if coordinationConditionChanged && coordinationCondition != nil {
				eventType := corev1.EventTypeNormal
				if coordinationCondition.Status == metav1.ConditionTrue {
					eventType = corev1.EventTypeWarning
				}
				c.emitRoleStatusEvent(latestMS, eventType, coordinationCondition.Reason, coordinationCondition.Message)
			}
			// Clean up old revisions only after roles have been updated (revision status changed)
			if revisionUpdated && !progressActive && !rolloutActive && !hasProtectedReplicas(latestMS) && c.revisionHistory(ctx, ms).errors() == nil {
				if cleanupErr := utils.CleanupOldControllerRevisions(context.TODO(), c.kubeClientSet, copy, referencedRevisions...); cleanupErr != nil {
					klog.Warningf("Failed to cleanup old ControllerRevisions after updating revision status for ModelServing %s/%s: %v", copy.Namespace, copy.Name, cleanupErr)
				}
			}
		}

		return nil
	})
}

// Without a status schema extension, retain history conservatively while any
// partition can require an absent ordinal's old revision after a restart.
func hasProtectedReplicas(ms *workloadv1alpha1.ModelServing) bool {
	if partition := modelServingPartition(ms); partition != nil && partition.String() != "0" && partition.String() != "0%" {
		return true
	}
	for _, role := range ms.Spec.Template.Roles {
		if partition := rolePartition(ms, role); partition != nil && partition.String() != "0" && partition.String() != "0%" {
			return true
		}
	}
	return false
}

// getPartition resolves an absolute partition from the configuration selected
// for the active rollout granularity.
// Returns (0, false, nil) when partition is not configured.
// If partition is a percentage, it is calculated from replicas (rounded up).
func (c *ModelServingController) getPartition(partitionConfig *intstr.IntOrString, replicas int) (int, bool, error) {
	if partitionConfig == nil {
		return 0, false, nil
	}
	// Percentage partition requires replicas to compute the absolute value.
	partition, err := intstr.GetScaledValueFromIntOrPercent(partitionConfig, replicas, true)
	if err != nil {
		return 0, true, err
	}
	return partition, true, nil
}

// partitionProtected reports whether an instance's absolute ordinal is below
// partition. Unparsable ordinals are filtered when objects are observed.
func partitionProtected(name string, partition int) bool {
	_, ordinal := utils.GetParentNameAndOrdinal(name)
	return ordinal < partition
}

func modelServingPartition(ms *workloadv1alpha1.ModelServing) *intstr.IntOrString {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate || ms.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		return nil
	}
	return ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition
}

func rolePartition(ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role) *intstr.IntOrString {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return nil
	}
	return role.Partition
}

func modelServingReplicas(ms *workloadv1alpha1.ModelServing) int {
	if ms.Spec.Replicas == nil {
		return 0
	}
	return int(*ms.Spec.Replicas)
}

func roleReplicas(role workloadv1alpha1.Role) int {
	if role.Replicas == nil {
		return 1
	}
	return int(*role.Replicas)
}

// forEachMissingOrdinal visits at most limit missing ordinals in [0, expectedCount).
// Memory usage depends on the observed ordinals rather than the user-provided
// expectedCount, which may be as large as math.MaxInt32.
// Invalid, out-of-range, duplicate, and unsorted existing ordinals are tolerated.
func forEachMissingOrdinal(expectedCount int, existingOrdinals []int, limit int, handler func(int) bool) {
	if expectedCount <= 0 || limit <= 0 {
		return
	}

	existing := make(map[int]struct{}, min(len(existingOrdinals), expectedCount))
	for _, ordinal := range existingOrdinals {
		if ordinal >= 0 && ordinal < expectedCount {
			existing[ordinal] = struct{}{}
		}
	}

	visited := 0
	for ordinal := 0; ordinal < expectedCount && visited < limit; ordinal++ {
		if _, found := existing[ordinal]; found {
			continue
		}
		visited++
		if !handler(ordinal) {
			return
		}
	}
}

// scaleDownServingGroups scales down the ServingGroups to the expected count with two-level priority-based selection:
// 1. Primary: Not-ready groups (Creating, NotFound) are deleted first
// 2. Secondary: Among groups with same status, lower deletion cost = delete first
// Partition protects templates, not explicit replica-count decreases.
func (c *ModelServingController) scaleDownServingGroups(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupList []datastore.ServingGroup, expectedCount int) error {
	// Calculate scores for all servingGroups first
	allScores := make([]ServingGroupWithScore, 0, len(servingGroupList))
	for _, group := range servingGroupList {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		scoreInfo := c.calculateServingGroupScore(ms, group.Name)
		allScores = append(allScores, scoreInfo)
	}

	// Sort by priority tuple: (priority, deletionCost, index).
	// Lower priority value = higher deletion priority (delete first)
	// Lower deletion cost = higher deletion priority
	// Higher index = higher deletion priority (backward compatibility)
	sortGroups := func(a, b ServingGroupWithScore) int {
		// Primary: Sort by priority (not-ready first)
		if a.Priority != b.Priority {
			return cmp.Compare(a.Priority, b.Priority) // Ascending: lower priority (not-ready) first
		}

		// Secondary: Among groups with same priority, lower deletion cost comes first
		if a.DeletionCost != b.DeletionCost {
			return cmp.Compare(a.DeletionCost, b.DeletionCost) // Ascending: lower cost first
		}

		// Tertiary: Higher index comes first (backward compatibility)
		return cmp.Compare(b.Index, a.Index) // Descending: higher indices first
	}

	slices.SortFunc(allScores, sortGroups)

	totalToDelete := max(0, len(allScores)-expectedCount)

	var err []error
	for i := 0; i < totalToDelete; i++ {
		targetGroup := allScores[i]
		klog.V(2).Infof("Scaling down serving group %s (priority: %d, deletion cost: %d, index: %d)",
			targetGroup.Name, targetGroup.Priority, targetGroup.DeletionCost, targetGroup.Index)
		if e := c.deleteServingGroup(ctx, ms, targetGroup.Name); e != nil {
			err = append(err, e)
		}
	}

	if len(err) > 0 {
		return errors.Join(err...)
	}

	return nil
}

func (c *ModelServingController) buildPluginChain(ms *workloadv1alpha1.ModelServing) (*plugins.Chain, error) {
	if ms == nil || len(ms.Spec.Plugins) == 0 {
		return nil, nil
	}
	if c.pluginsRegistry == nil {
		return nil, fmt.Errorf("plugin registry is not initialized")
	}
	return plugins.NewChain(c.pluginsRegistry, ms.Spec.Plugins)
}

func (c *ModelServingController) runRoleDeletePlugins(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) error {
	if c.pluginsRegistry == nil {
		return fmt.Errorf("plugin registry is not initialized")
	}
	chain, err := plugins.NewRoleDeleteChain(c.pluginsRegistry, ms.Spec.Plugins)
	if err != nil {
		return fmt.Errorf("build Role delete plugin chain: %w", err)
	}
	_, roleIndex := utils.GetParentNameAndOrdinal(roleID)
	var role *workloadv1alpha1.Role
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			role = ms.Spec.Template.Roles[i].DeepCopy()
			break
		}
	}
	cleanup, _ := c.roleCleanups.LoadOrStore(auxiliaryCleanupKey{ms.UID, groupName, roleName, roleID}, &plugins.RoleCleanup{})
	return chain.OnRoleDelete(ctx, &plugins.HookRequest{
		Cleanup:       cleanup.(*plugins.RoleCleanup),
		ModelServing:  ms,
		ServingGroup:  groupName,
		RoleName:      roleName,
		RoleID:        roleID,
		RoleIndex:     roleIndex,
		Role:          role,
		KubeClient:    c.kubeClientSet,
		ServiceLister: c.servicesLister,
	})
}

func (c *ModelServingController) CreatePodsForServingGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupIndex int, revision string, roles []workloadv1alpha1.Role) error {
	servingGroupName := utils.GenerateServingGroupName(ms.Name, servingGroupIndex)
	for _, role := range roles {
		roleTemplateHash, err := utils.RoleRevisionHash(ms, role.Name)
		if err != nil {
			return err
		}
		replicas := int(*role.Replicas)
		for i := 0; i < replicas; i++ {
			appliedHash, err := c.createPodsByRole(ctx, *role.DeepCopy(), ms, i, servingGroupIndex, revision, roleTemplateHash)
			if err != nil {
				return err
			}
			roleID := utils.GenerateRoleID(role.Name, i)
			c.store.AddRole(utils.GetNamespaceName(ms), servingGroupName, role.Name, roleID, revision, appliedHash)
			// Emit event for new role entering Creating state
			message := fmt.Sprintf("Role %s/%s in ServingGroup %s is now Creating", role.Name, roleID, servingGroupName)
			c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "RoleCreating", message)
		}
	}
	return nil
}

func (c *ModelServingController) CreatePodsByRole(ctx context.Context, role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, roleIndex int, servingGroupOrdinal int, revision string, roleTemplateHash string) error {
	_, err := c.createPodsByRole(ctx, role, ms, roleIndex, servingGroupOrdinal, revision, roleTemplateHash)
	return err
}

// Return the hash actually rendered so new Role store records and Pod identity
// agree even when a v1 snapshot normalizes an otherwise equivalent template.
func (c *ModelServingController) createPodsByRole(ctx context.Context, role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, roleIndex int, servingGroupOrdinal int, revision string, _ string) (string, error) {
	if revision == c.revisionTarget(ctx, ms) {
		if err := c.ensureTargetRevision(ctx, ms, revision); err != nil {
			return "", err
		}
	}
	applicable, err := c.modelServingForRevision(ctx, ms, revision)
	if err != nil {
		return "", err
	}
	role, err = historicalRole(applicable, role.Name, revision)
	if err != nil {
		return "", err
	}
	hash, err := utils.RoleRevisionHash(applicable, role.Name)
	if err != nil {
		return "", err
	}
	chain, err := c.buildPluginChain(applicable)
	if err != nil {
		return "", err
	}
	return hash, c.createPodsByRoleWithContext(ctx, role, applicable, roleIndex, servingGroupOrdinal, revision, hash, chain)
}

func (c *ModelServingController) createPodsByRoleWithContext(ctx context.Context, role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, roleIndex, servingGroupOrdinal int, revision, roleTemplateHash string, chain *plugins.Chain) error {
	// Pod rendering mutates nested container fields; never alter revision inputs.
	role = *role.DeepCopy()
	servingGroupName := utils.GenerateServingGroupName(ms.Name, servingGroupOrdinal)
	roleID := utils.GenerateRoleID(role.Name, roleIndex)
	entryPod := utils.GenerateEntryPod(role, ms, servingGroupName, roleID, revision, roleTemplateHash)
	taskName := c.podGroupManager.GenerateTaskName(role.Name, roleIndex)
	c.podGroupManager.AnnotatePodWithPodGroup(entryPod, ms, servingGroupName, taskName)
	if err := c.createPod(ctx, ms, servingGroupName, role.Name, roleID, role.DeepCopy(), entryPod, true, chain, "entry"); err != nil {
		return err
	}
	if roleCoordination(ms) != nil {
		// The accepted entry has already started this Role, even if a worker
		// creation fails. Keep it in the rollout ledger and retry its missing
		// members rather than issuing another start or skipping its ordinal.
		c.store.AddRole(utils.GetNamespaceName(ms), servingGroupName, role.Name, roleID, revision, roleTemplateHash)
	}
	if role.WorkerReplicas > 0 && role.WorkerTemplate == nil {
		klog.Errorf("WorkerTemplate is required when workerReplicas > 0 for role %s. This should have been caught by webhook validation.", role.Name)
		return nil
	}

	for i := 1; i <= int(role.WorkerReplicas); i++ {
		workerPod := utils.GenerateWorkerPod(role, ms, servingGroupName, roleID, i, revision, roleTemplateHash)
		c.podGroupManager.AnnotatePodWithPodGroup(workerPod, ms, servingGroupName, taskName)
		if err := c.createPod(ctx, ms, servingGroupName, role.Name, roleID, role.DeepCopy(), workerPod, false, chain, "worker"); err != nil {
			return err
		}
	}
	return nil
}

func (c *ModelServingController) createPod(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroupName string,
	roleName string,
	roleID string,
	role *workloadv1alpha1.Role,
	pod *corev1.Pod,
	isEntry bool,
	chain *plugins.Chain,
	roleKind string,
) error {
	if chain != nil {
		req := &plugins.HookRequest{
			ModelServing:  ms,
			ServingGroup:  servingGroupName,
			RoleName:      roleName,
			RoleID:        roleID,
			Role:          role,
			IsEntry:       isEntry,
			Pod:           pod,
			KubeClient:    c.kubeClientSet,
			ServiceLister: c.servicesLister,
		}
		if err := chain.OnPodCreate(ctx, req); err != nil {
			c.emitRoleStatusEvent(ms, corev1.EventTypeWarning, "PluginError", err.Error())
			return fmt.Errorf("execute OnPodCreate failed for %s pod %s: %v", roleKind, pod.Name, err)
		}
	}

	if roleCoordination(ms) != nil {
		delete(pod.Annotations, surgeAnnotation)
		temporary, _ := ctx.Value(coordinatedSurgeKey{}).(bool)
		if !temporary && c.podsInformer != nil {
			existing, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, servingGroupName, roleName, roleID))
			if err != nil {
				return err
			}
			for _, sibling := range existing {
				if utils.IsOwnedByModelServingWithUID(sibling, ms.UID) && sibling.Annotations[surgeAnnotation] == surgeRole {
					temporary = true
				}
			}
		}
		if temporary {
			setSurgeScope(pod, surgeRole)
		}
	}
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing, getErr := c.podsLister.Pods(ms.Namespace).Get(pod.Name)
			if getErr != nil || (roleCoordination(ms) != nil && ms.ResourceVersion != "") {
				// A stale lister may still contain the previous owned incarnation.
				// AlreadyExists describes the API object, which must be verified.
				existing, getErr = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			}
			if getErr != nil {
				return fmt.Errorf("failed to verify existing %s pod %s: %v", roleKind, pod.Name, getErr)
			}
			existingRoleTemplateHash := utils.ObjectRoleTemplateHash(existing)
			expectedRoleTemplateHash := utils.ObjectRoleTemplateHash(pod)
			roleTemplateHashMatches := existingRoleTemplateHash == "" || expectedRoleTemplateHash == "" || existingRoleTemplateHash == expectedRoleTemplateHash
			if !roleTemplateHashMatches {
				historical, historyErr := c.revisionHistory(ctx, ms).workload(ctx, utils.ObjectRevision(existing))
				if historyErr == nil {
					// ms can be a protected historical workload. Do not use the
					// reconcile cache's comparisons against the current desired spec.
					equal, compareErr := utils.EqualRoleRevisions(existingRoleTemplateHash, expectedRoleTemplateHash, historical, ms, roleName)
					roleTemplateHashMatches = compareErr == nil && equal
				}
			}
			state := inplace.StateOf(existing)
			completedTarget := state != nil && state.Done && state.Revision != "" && state.Revision == c.revisionTarget(ctx, ms) && utils.ObjectRevision(existing) == state.Revision
			identityMatches := utils.IsOwnedByModelServingWithUID(existing, ms.UID) &&
				existing.Labels[workloadv1alpha1.GroupNameLabelKey] == servingGroupName &&
				existing.Labels[workloadv1alpha1.RoleLabelKey] == roleName &&
				existing.Labels[workloadv1alpha1.RoleIDKey] == roleID &&
				((utils.ObjectRevision(existing) == utils.ObjectRevision(pod) && roleTemplateHashMatches) || completedTarget)
			if !identityMatches {
				c.enqueueModelServingAfter(ms, enqueueAfter)
				return fmt.Errorf("existing %s pod %s does not match expected identity: owner=%t group=%q/%q role=%q/%q roleID=%q/%q revision=%q/%q roleTemplateHash=%q/%q",
					roleKind, pod.Name,
					utils.IsOwnedByModelServingWithUID(existing, ms.UID),
					existing.Labels[workloadv1alpha1.GroupNameLabelKey], servingGroupName,
					existing.Labels[workloadv1alpha1.RoleLabelKey], roleName,
					existing.Labels[workloadv1alpha1.RoleIDKey], roleID,
					utils.ObjectRevision(existing), utils.ObjectRevision(pod),
					existingRoleTemplateHash, expectedRoleTemplateHash)
			}
		} else {
			return fmt.Errorf("failed to create %s pod %s: %v", roleKind, pod.Name, err)
		}
	}

	return nil
}

func (c *ModelServingController) deleteServingGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) error {
	status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName)
	if status == datastore.ServingGroupNotFound {
		return nil
	}

	rolesByName, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), servingGroupName)
	if err != nil {
		return fmt.Errorf("get Roles for ServingGroup %s during deletion: %w", servingGroupName, err)
	}
	// Mark the ServingGroup deleting before deleting its Role resources so
	// active-role reconciliation cannot recreate them during teardown.
	err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupDeleting)
	if err != nil {
		klog.ErrorS(err, "Failed to update ServingGroup status", "namespace", ms.Namespace, "servingGroup", servingGroupName)
		return err
	}
	defer func() {
		if err != nil {
			c.enqueueModelServing(ms)
		}
	}()

	for roleName, roles := range rolesByName {
		for roleID := range roles {
			if err = c.prepareRoleCleanup(ctx, ms, servingGroupName, roleName, roleID); err != nil {
				return err
			}
		}
	}
	err = c.podGroupManager.DeletePodGroup(ctx, ms, servingGroupName)
	if err != nil {
		return fmt.Errorf("failed to delete PodGroup for ServingGroup %s: %v", servingGroupName, err)
	}

	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.GroupNameLabelKey: servingGroupName,
	})
	podTargets, _ := c.podDeletions.LoadOrStore(auxiliaryCleanupKey{uid: ms.UID, group: servingGroupName}, &podDeletionTargets{})
	err = podTargets.(*podDeletionTargets).Delete(ctx, c.kubeClientSet, ms, selector)
	if err != nil {
		return fmt.Errorf("failed to delete pods of ServingGroup %s: %v", servingGroupName, err)
	}

	if names, getErr := podTargets.(*podDeletionTargets).replacements(ctx, c.kubeClientSet, ms.Namespace, selector); getErr != nil {
		return getErr
	} else if len(names) > 0 {
		c.emitRoleStatusEvent(ms, corev1.EventTypeWarning, "DeletionIdentityChanged", fmt.Sprintf("Preserving replacement Pods %v; waiting for ServingGroup identity reconciliation", names))
		return nil
	}

	for roleName, roles := range rolesByName {
		for roleID := range roles {
			if err = c.runRoleDeletePlugins(ctx, ms, servingGroupName, roleName, roleID); err != nil {
				return err
			}
		}
	}

	if c.isServingGroupDeleted(ms, servingGroupName) {
		klog.V(2).Infof("ServingGroup %s has been deleted", servingGroupName)
		c.forgetGroupCleanup(ms.UID, servingGroupName)
		c.store.DeleteServingGroup(utils.GetNamespaceName(ms), servingGroupName)
		c.podDeletions.Delete(auxiliaryCleanupKey{uid: ms.UID, group: servingGroupName})
		// this is needed when a pod is deleted accidentally, and the ServingGroup is deleted completely
		// and the controller has no chance to supplement it.
		c.enqueueModelServing(ms)
	}
	return nil
}

func (c *ModelServingController) createOrUpdatePodGroupByServingGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) error {
	podGroupMS := ms
	if roleCoordination(ms) != nil {
		var err error
		podGroupMS, err = c.coordinatedPodGroupLayoutInRound(ctx, ms, servingGroupName)
		if err != nil {
			return err
		}
		if err := c.stageCoordinatedExpansionGang(ctx, ms, podGroupMS, servingGroupName); err != nil {
			return err
		}
	}
	if err, retryAfter := c.podGroupManager.CreateOrUpdatePodGroup(ctx, podGroupMS, servingGroupName); err != nil {
		if retryAfter > 0 {
			klog.V(2).Infof("Retry syncing modelserving %s after %v: %v", servingGroupName, retryAfter, err)
			c.enqueueModelServingAfter(ms, retryAfter)
			return nil
		}
		return fmt.Errorf("failed to update PodGroup for ServingGroup %s: %v", servingGroupName, err)
	}
	return nil
}

// resolveRoleTemplateHash resolves role template hash from labels first.
// For legacy pods without roleTemplateHash label, fallback to:
// 1. get pod revision from labels
// 2. find corresponding ControllerRevision
// 3. hash the matched role from ControllerRevision
// If any step fails, return empty string and let rolling update handle reconciliation.
func (c *ModelServingController) resolveRoleTemplateHash(ms *workloadv1alpha1.ModelServing, roleName string, obj metav1.Object) string {
	roleTemplateHash := utils.ObjectRoleTemplateHash(obj)
	if roleTemplateHash != "" {
		return roleTemplateHash
	}

	revision := utils.ObjectRevision(obj)
	if revision == "" {
		klog.V(4).Infof("roleTemplateHash and revision labels are missing on object %s/%s, leave roleTemplateHash empty", obj.GetNamespace(), obj.GetName())
		return ""
	}

	if c == nil || c.kubeClientSet == nil {
		klog.V(4).Infof("kube client is nil when resolving roleTemplateHash for object %s/%s, leave empty", obj.GetNamespace(), obj.GetName())
		return ""
	}

	resolvedHash, ok := c.resolveRoleTemplateHashFromRevision(ms, revision, roleName)
	if ok {
		return resolvedHash
	}

	klog.V(4).Infof("role %s not found in ControllerRevision %s for ModelServing %s/%s, leave roleTemplateHash empty", roleName, revision, ms.Namespace, ms.Name)
	return ""
}

// resolveRoleTemplateHashFromRevision resolves roleTemplateHash from a revision's ControllerRevision.
// Returns (hash, true) when resolved, otherwise ("", false).
func (c *ModelServingController) resolveRoleTemplateHashFromRevision(ms *workloadv1alpha1.ModelServing, revision, roleName string) (string, bool) {
	if c == nil || c.kubeClientSet == nil || revision == "" {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil {
		klog.Warningf("failed to get ControllerRevision %s for ModelServing %s/%s: %v", revision, ms.Namespace, ms.Name, err)
		return "", false
	}
	if cr == nil {
		return "", false
	}

	workload, err := utils.ModelServingForControllerRevision(ms, cr)
	if err != nil {
		klog.Warningf("failed to parse roles from ControllerRevision %s for ModelServing %s/%s: %v", revision, ms.Namespace, ms.Name, err)
		return "", false
	}
	hash, err := utils.RoleRevisionHash(workload, roleName)
	return hash, err == nil
}

// resolveRoleTemplateHashForComparison is retained for callers outside a
// reconcile context. Production reconciliation passes its shared history cache
// directly to roleTemplateHashForComparison.
func (c *ModelServingController) resolveRoleTemplateHashForComparison(
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleName string,
	role datastore.Role,
) (string, bool) {
	ctx := c.withRevisionHistory(context.Background(), ms)
	return c.roleTemplateHashForComparison(ctx, ms, servingGroup, roleName, role)
}

// findOutdatedRolesInServingGroups finds outdated roles in serving groups and returns a map of serving group names to outdated role names
// If a serving group has no outdated roles, it updates the serving group's revision in the store
func (c *ModelServingController) findOutdatedRolesInServingGroups(ms *workloadv1alpha1.ModelServing, servingGroups []datastore.ServingGroup, revision string) map[string][]string {
	outdatedRolesMap := make(map[string][]string)
	ctx := c.withRevisionHistory(context.Background(), ms)

	// Create a mapping of role name to expected role revision based on current spec
	expectedroleTemplateHashs := make(map[string]string)
	newRoleNames := make(map[string]bool)
	for _, role := range ms.Spec.Template.Roles {
		hash, err := c.revisionHistory(ctx, ms).roleHash(role.Name)
		if err != nil {
			klog.Errorf("cannot calculate Role revision: %v", err)
			return outdatedRolesMap
		}
		expectedroleTemplateHashs[role.Name] = hash
		newRoleNames[role.Name] = true
	}

	for _, sg := range servingGroups {
		var outdatedRoleNames []string
		unresolved := false

		// Check each role in the current serving group
		for roleName, roleTemplateHash := range expectedroleTemplateHashs {
			// Get a safe copy of the roles list from the store to avoid concurrent map iteration/write.
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), sg.Name, roleName)
			if err != nil {
				klog.Errorf("failed to get roles for ServingGroup %s, role %s: %v", sg.Name, roleName, err)
				continue
			}

			// Check if any instance of this role type is outdated
			hasOutdatedRole := false
			roleUnresolved := false
			for _, role := range roles {
				observedRoleTemplateHash, ok := c.roleTemplateHashForComparison(ctx, ms, sg, roleName, role)
				if !ok {
					// Legacy upgrade compatibility: missing roleTemplateHash should not trigger forced restart
					// when we cannot safely infer historical template.
					klog.Warningf("skip outdated check for role %s/%s in ServingGroup %s because roleTemplateHash is missing and cannot be inferred", roleName, role.Name, sg.Name)
					unresolved, roleUnresolved = true, true
					continue
				}
				// If the role revision in the store is different from the expected revision and
				// the role is not already being deleted, it's outdated
				if observedRoleTemplateHash != roleTemplateHash && role.Status != datastore.RoleDeleting {
					hasOutdatedRole = true
					break
				}
			}

			if hasOutdatedRole && !roleUnresolved {
				outdatedRoleNames = append(outdatedRoleNames, roleName)
			}
		}

		// Additionally, check for roles that exist in the store but are not in the new spec
		// These roles should also be considered "outdated" and need to be deleted
		allRoles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), sg.Name)
		if err != nil {
			klog.Errorf("failed to get all roles for ServingGroup %s: %v", sg.Name, err)
			continue
		}
		for storedRoleName := range allRoles {
			if !newRoleNames[storedRoleName] {
				// This role exists in the store but not in the new spec, so it's outdated
				outdatedRoleNames = append(outdatedRoleNames, storedRoleName)
			}
		}

		if len(outdatedRoleNames) > 0 {
			// There are outdated roles in this serving group
			outdatedRolesMap[sg.Name] = outdatedRoleNames
		} else if !unresolved {
			// No outdated roles in this serving group, update its revision in the store
			err := c.store.UpdateServingGroupRevision(utils.GetNamespaceName(ms), sg.Name, revision)
			if err != nil {
				klog.Errorf("failed to update ServingGroup %s revision: %v", sg.Name, err)
			} else {
				klog.V(2).Infof("Updated ServingGroup %s revision to latest: %s", sg.Name, revision)
			}
		}
	}

	return outdatedRolesMap
}

// handleModelServingDatastoreCacheDump handles requests to dump the ServingGroup and role in dataStore cache.
func (c *ModelServingController) handleModelServingDatastoreCacheDump(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, err := c.store.DumpCache()
	if err != nil {
		klog.Errorf("failed to dump model serving datastore cache: %v", err)
		http.Error(w, "Failed to dump datastore cache", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(data); err != nil {
		klog.Errorf("failed to write cache dump response: %v", err)
	}
}

// RegisterModelServingDebugEndpoints registers debug endpoints for the ModelServingController
func (c *ModelServingController) RegisterModelServingDebugEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/debug/modelserving/cache", c.handleModelServingDatastoreCacheDump)
}

func (c *ModelServingController) hasUpdateableOutdatedRole(
	ms *workloadv1alpha1.ModelServing,
	groupName string,
	targetRole workloadv1alpha1.Role,
	roleList []datastore.Role,
) bool {
	return c.hasUpdateableOutdatedRoleInRound(context.Background(), ms, groupName, targetRole, roleList)
}

func (c *ModelServingController) updateModelServingStatus(
	ms *workloadv1alpha1.ModelServing,
	revision string,
	rolloutPolicy *roleRolloutPolicy,
) error {
	return c.updateModelServingStatusInRound(context.Background(), ms, revision, rolloutPolicy)
}

func (c *ModelServingController) checkRoleReady(ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) (bool, error) {
	return c.checkRoleReadyInRound(context.Background(), ms, servingGroupName, roleName, roleID)
}

func (c *ModelServingController) outdatedRoles(ms *workloadv1alpha1.ModelServing, sg datastore.ServingGroup, roleSpec workloadv1alpha1.Role, roleList []datastore.Role) ([]datastore.Role, int) {
	return c.outdatedRolesInRound(context.Background(), ms, sg, roleSpec, roleList)
}

func (c *ModelServingController) rolesToDeleteForRoleRollingUpdate(
	ms *workloadv1alpha1.ModelServing,
	sg datastore.ServingGroup,
	groupPolicy *roleRolloutGroupPolicy,
) ([]roleToDelete, bool, error) {
	return c.rolesToDeleteForRoleRollingUpdateInRound(context.Background(), ms, sg, groupPolicy)
}

func (c *ModelServingController) manageRollingUpdate(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision string) error {
	policy, err := c.resolveRoleRolloutPolicy(ctx, ms, revision)
	if err != nil {
		return err
	}
	return c.manageRollingUpdateWithPolicy(ctx, ms, revision, policy)
}
