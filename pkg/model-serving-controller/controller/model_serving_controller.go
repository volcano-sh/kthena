/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permsssions and
limstations under the License.
*/

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextClientSet "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	volcano "volcano.sh/apis/pkg/client/clientset/versioned"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	GroupNameKey = "GroupName"
	RoleIDKey    = "RoleID"
)

type ModelServingController struct {
	kubeClientSet      kubernetes.Interface
	modelServingClient clientset.Interface

	syncHandler           func(ctx context.Context, msKey string) error
	podGroupManager       *podgroupmanager.Manager
	podsLister            listerv1.PodLister
	podsInformer          cache.SharedIndexInformer
	servicesLister        listerv1.ServiceLister
	servicesInformer      cache.SharedIndexInformer
	modelServingLister    listerv1alpha1.ModelServingLister
	modelServingsInformer cache.SharedIndexInformer

	// nolint
	workqueue       workqueue.RateLimitingInterface
	store           datastore.Store
	graceMap        sync.Map // key: errorPod.namespace/errorPod.name, value:time
	initialSync     bool     // indicates whether the initial sync has been completed
	pluginsRegistry *plugins.Registry
}

func NewModelServingController(kubeClientSet kubernetes.Interface, modelServingClient clientset.Interface, volcanoClient volcano.Interface, apiextClient apiextClientSet.Interface) (*ModelServingController, error) {
	selector, err := labels.NewRequirement(workloadv1alpha1.GroupNameLabelKey, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create label selector, err: %v", err)
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
	c := &ModelServingController{
		kubeClientSet:         kubeClientSet,
		modelServingClient:    modelServingClient,
		podGroupManager:       podgroupmanager.NewManager(kubeClientSet, volcanoClient, apiextClient, store),
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
	}

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
		klog.Error("failed to parse ModelServing type when updatems")
		return
	}
	oldms, ok := old.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Error("failed to parse ModelServing type when updatems")
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
		if curms.Spec.Template.GangPolicy.MinRoleReplicas == nil {
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
		// If the object is not a ModelServing, it msght be a tombstone object.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("failed to parse ModelServing type when deletems %#v", obj)
			return
		}
		ms, ok = tombstone.Obj.(*workloadv1alpha1.ModelServing)
		if !ok {
			klog.Errorf("failed to parse ModelServing from tombstone %#v", tombstone.Obj)
			return
		}
	}

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
			klog.Errorf("get model Serving failed when update pod: %v", err)
		}
		return
	}

	if c.shouldSkipPodHandling(ms, servingGroupName, newPod) {
		// Pod revision mssmatch ServingGroup, this can rarely happen
		return
	}

	switch {
	case utils.IsPodRunningAndReady(newPod):
		// The pod is available, that is, the state is running, and the container is ready
		err = c.handleReadyPod(ms, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle running pod failed: %v", err)
		}
	case utils.IsPodFailed(newPod) || utils.ContainerRestarted(newPod):
		// handleErrorPod is not called until modelServing has been called.
		if !c.initialSync {
			return
		}
		// Failure occurs in pod and we need to wait for a grace period before making a judgment.
		err = c.handleErrorPod(ms, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle error pod failed: %v", err)
		}
	}
}

func (c *ModelServingController) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// If the object is not a Pod, it msght be a tombstone object.
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
		return
	}
	// Remove the pod from running pods in the store
	c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), servingGroupName, pod.Name)

	// skip handling if pod revision mismatches serving group revision
	if c.shouldSkipPodHandling(ms, servingGroupName, pod) {
		return
	}

	if c.handleDeletionInProgress(ms, servingGroupName, roleName, roleID) {
		return
	}

	err := c.handleDeletedPod(ms, servingGroupName, pod)
	if err != nil {
		klog.Errorf("handle deleted pod failed: %v", err)
	}
}

func (c *ModelServingController) deleteService(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		// If the object is not a Service, it msght be a tombstone object.
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
		return
	}

	if c.handleDeletionInProgress(ms, servingGroupName, roleName, roleID) {
		return
	}

	klog.V(4).Infof("Service %s/%s deleted, enqueuing ModelServing %s for reconcile", svc.GetNamespace(), svc.GetName(), ms.Name)
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

func (c *ModelServingController) syncModelServing(ctx context.Context, key string) error {
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

	// only fields in roles can be modified in rolling updates.
	// and only modifying the role.replicas field will not affect the revision.
	copy := utils.RemoveRoleReplicasForRevision(ms)
	revision := utils.Revision(copy.Spec.Template.Roles)
	if err := c.manageServingGroupReplicas(ctx, ms, revision); err != nil {
		return fmt.Errorf("cannot manage ServingGroup replicas: %v", err)
	}

	if err := c.manageRole(ctx, ms, revision); err != nil {
		return fmt.Errorf("cannot manage role replicas: %v", err)
	}

	if err := c.manageServingGroupRollingUpdate(ctx, ms, revision); err != nil {
		return fmt.Errorf("cannot manage ServingGroup rollingUpdate: %v", err)
	}

	if err := c.manageHeadlessService(ctx, ms); err != nil {
		return fmt.Errorf("cannot manage ModelServing: %v", err)
	}

	if err := c.UpdateModelServingStatus(ms, revision); err != nil {
		return fmt.Errorf("failed to update status of ms %s/%s: %v", namespace, name, err)
	}

	return nil
}

func (c *ModelServingController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// start informers
	go c.podsInformer.RunWithContext(ctx)
	go c.servicesInformer.RunWithContext(ctx)
	go c.modelServingsInformer.RunWithContext(ctx)
	go c.podGroupManager.CrdInformer.RunWithContext(ctx)

	cache.WaitForCacheSync(ctx.Done(),
		c.podsInformer.HasSynced,
		c.servicesInformer.HasSynced,
		c.modelServingsInformer.HasSynced,
		c.podGroupManager.CrdInformer.HasSynced,
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
	c.initialSync = true
}

func (c *ModelServingController) manageServingGroupReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, newRevision string) error {
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get servingGroup of modelServing: %s from map: %v", ms.GetName(), err)
	}
	expectedCount := int(*ms.Spec.Replicas)
	curReplicas := len(servingGroupList)

	// Determsne whether it is a scale-up or scale-down scenario
	if curReplicas < expectedCount {
		// update pod groups if needed
		for _, servingGroup := range servingGroupList {
			if err := c.podGroupManager.CreateOrUpdatePodGroup(ctx, ms, servingGroup.Name); err != nil {
				klog.Errorf("failed to update PodGroup for ServingGroup %s: %v", servingGroup.Name, err)
			}
		}
		if err := c.scaleUpServingGroups(ctx, ms, servingGroupList, expectedCount, newRevision); err != nil {
			return fmt.Errorf("failed to scale up ServingGroups: %v", err)
		}
	} else {
		if curReplicas > expectedCount {
			if err := c.scaleDownServingGroups(ctx, ms, servingGroupList, expectedCount); err != nil {
				return fmt.Errorf("failed to scale down ServingGroups: %v", err)
			}
		}

		// Note: in case the role is updated, we need to update pod groups as well.
		// update pod group after scaling down, so that we do not need to update pod group for deleting serving groups
		servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
		if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
			return fmt.Errorf("cannot get servingGroup of modelServing: %s from map: %v", ms.GetName(), err)
		}
		for _, servingGroup := range servingGroupList {
			if servingGroup.Status != datastore.ServingGroupDeleting {
				if err := c.podGroupManager.CreateOrUpdatePodGroup(ctx, ms, servingGroup.Name); err != nil {
					klog.Errorf("failed to update PodGroup for ServingGroup %s: %v", servingGroup.Name, err)
				}
			}
		}
	}
	return nil
}

// scaleUpServingGroups scales up the ServingGroups to the expected count.
// When partition is set, it fills missing ordinals in [0, partition) using CurrentRevision.
// Otherwise, it creates new ServingGroups with increasing indices starting from the current max index + 1.
func (c *ModelServingController) scaleUpServingGroups(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupList []datastore.ServingGroup, expectedCount int, newRevision string) error {
	partition := c.getPartition(ms)

	// Find the maximum ordinal in existing servingGroups
	// Since servingGroupList is already sorted in ascending order by ordinal,
	// we can directly get the maxOrdinal from the last element
	maxOrdinal := -1
	existingOrdinals := make(map[int]bool)
	for _, group := range servingGroupList {
		_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
		existingOrdinals[ordinal] = true
	}
	// Get maxOrdinal from the last element (list is sorted in ascending order)
	if len(servingGroupList) > 0 {
		_, maxOrdinal = utils.GetParentNameAndOrdinal(servingGroupList[len(servingGroupList)-1].Name)
	}

	// Helper function to create a ServingGroup
	createServingGroup := func(ordinal int, revision string, roles []workloadv1alpha1.Role) error {
		groupName := utils.GenerateServingGroupName(ms.Name, ordinal)
		// Ensure a PodGroup exists for the new ServingGroup when gang scheduling is enabled.
		if err := c.podGroupManager.CreateOrUpdatePodGroup(ctx, ms, groupName); err != nil {
			return err
		}
		// Create pods for ServingGroup using the provided roles template
		if err := c.CreatePodsForServingGroup(ctx, ms, ordinal, revision, roles); err != nil {
			return fmt.Errorf("create Serving group failed: %v", err)
		}
		// Insert new ServingGroup to global storage
		c.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, revision)
		return nil
	}

	if partition > 0 {
		// When partition is set, fill missing ordinals in [0, partition) using CurrentRevision
		for ordinal := 0; ordinal < partition && ordinal < expectedCount; ordinal++ {
			if existingOrdinals[ordinal] {
				continue
			}

			// Use CurrentRevision for partition-protected ordinals
			revisionToUse := newRevision
			if ms.Status.CurrentRevision != "" {
				revisionToUse = ms.Status.CurrentRevision
			}

			// For ordinal < partition, we should use the old template from the revision
			// Two cases:
			// 1. First startup: use ms.Spec.Template.Roles (which corresponds to CurrentRevision)
			// 2. During recovery: use template from ControllerRevision retrieved by revision
			var rolesToUse []workloadv1alpha1.Role
			cr, _ := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revisionToUse)
			if cr != nil {
				// Case 2: Recovery scenario - use template from ControllerRevision
				if roles, err := utils.GetRolesFromControllerRevision(cr); err != nil {
					klog.Warningf("Failed to get roles from ControllerRevision for revision %s (ordinal %d): %v, falling back to ms.Spec.Template.Roles", revisionToUse, ordinal, err)
					rolesToUse = ms.Spec.Template.Roles
				} else {
					rolesToUse = roles
					klog.Infof("Recovering ServingGroup at ordinal %d with revision %s using template from ControllerRevision (partition=%d)", ordinal, revisionToUse, partition)
				}
			} else {
				// Case 1: First startup - ControllerRevision not found, use ms.Spec.Template.Roles
				rolesToUse = ms.Spec.Template.Roles
				klog.Infof("Creating missing ServingGroup at ordinal %d with revision %s using ms.Spec.Template.Roles (partition=%d, first startup)", ordinal, revisionToUse, partition)
			}

			if err := createServingGroup(ordinal, revisionToUse, rolesToUse); err != nil {
				return err
			}
			// Update existingOrdinals and maxOrdinal
			existingOrdinals[ordinal] = true
			if ordinal > maxOrdinal {
				maxOrdinal = ordinal
			}
		}
	}

	// Create new ServingGroups with increasing indices starting from the current max index + 1
	toCreate := expectedCount - len(existingOrdinals)

	if toCreate > 0 {
		startingIndex := maxOrdinal + 1

		// Create ControllerRevision when scaling up with a new revision
		// This is done once before the loop since newRevision and templateData are the same for all new groups
		templateData := ms.Spec.Template.Roles
		_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, newRevision, templateData)
		if err != nil {
			klog.Warningf("Failed to create ControllerRevision for new revision %s: %v", newRevision, err)
		}

		// Create new ServingGroups with increasing indices
		for i := startingIndex; i < startingIndex+toCreate; i++ {
			// For newly created ServingGroups (ordinal >= partition), always use current template
			if err := createServingGroup(i, newRevision, ms.Spec.Template.Roles); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *ModelServingController) manageRole(ctx context.Context, ms *workloadv1alpha1.ModelServing, newRevision string) error {
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get ServingGroup of modelServing: %s from map: %v", ms.GetName(), err)
	}
	for _, servingGroup := range servingGroupList {
		if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroup.Name) == datastore.ServingGroupDeleting {
			// Deleting ServingGroup will be recreated after the deletion is complete, so there is no need to scale the roles
			continue
		}
		_, servingGroupOrdinal := utils.GetParentNameAndOrdinal(servingGroup.Name)
		for _, targetRole := range ms.Spec.Template.Roles {
			c.manageRoleReplicas(ctx, ms, servingGroup.Name, targetRole, servingGroupOrdinal, newRevision)
		}
	}
	return nil
}

// scaleDownRoles handles Role scaling down with two-level priority-based selection:
// 1. Primary: Not-ready roles (Creating, NotFound) are deleted first
// 2. Secondary: Among roles with same status, lower deletion cost = delete first
func (c *ModelServingController) scaleDownRoles(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, roleList []datastore.Role, expectedCount int) {
	// Calculate priority information for all Roles
	var roleScores []RoleWithScore

	for _, role := range roleList {
		scoreInfo := c.calculateRoleScore(ms, groupName, targetRole.Name, role.Name)
		roleScores = append(roleScores, scoreInfo)
	}

	// Sort by priority tuple: (priority, deletionCost, index)
	// Lower priority value = higher deletion priority (delete first)
	// Lower deletion cost = higher deletion priority
	// Higher index = higher deletion priority (backward compatibility)
	slices.SortFunc(roleScores, func(a, b RoleWithScore) int {
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
	})

	// Role needs to scale down, and the ServingGroup status needs to be set to Scaling
	err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, datastore.ServingGroupScaling)
	if err != nil {
		klog.Errorf("failed to set ServingGroup %s/%s status: %v", ms.Namespace+"/"+ms.Name, groupName, err)
		return
	}

	// Delete from beginning (not-ready, low cost, high index first)
	numToDelete := len(roleScores) - expectedCount
	for i := 0; i < numToDelete; i++ {
		targetName := roleScores[i].Name
		klog.V(2).Infof("Scaling down role %s (priority: %d, deletion cost: %d, index: %d)",
			targetName, roleScores[i].Priority, roleScores[i].DeletionCost, roleScores[i].Index)
		c.DeleteRole(ctx, ms, groupName, targetRole.Name, targetName)
	}
}

// scaleUpRoles handles Role scaling up.
// It creates new Roles with increasing indices starting from the current max index + 1.
func (c *ModelServingController) scaleUpRoles(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, roleList []datastore.Role, expectedCount int, servingGroupOrdinal int, newRevision string) {
	startingIndex := 0
	if len(roleList) > 0 {
		_, ordinal := utils.GetParentNameAndOrdinal(roleList[len(roleList)-1].Name)
		// since roleList is already sorted in ascending order by index
		startingIndex = ordinal + 1
	}

	// Calculate how many new Roles we need to create
	toCreate := expectedCount - len(roleList)

	// Role needs to scale up, and the ServingGroup status needs to be set to Scaling
	err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, datastore.ServingGroupScaling)
	if err != nil {
		klog.Errorf("failed to set ServingGroup %s/%s status: %v", ms.Namespace+"/"+ms.Name, groupName, err)
		return
	}

	klog.V(2).Infof("Scaling up role %s in ServingGroup %s: creating %d new replicas", targetRole.Name, groupName, toCreate)
	// Create new Roles with increasing indices
	for i := 0; i < toCreate; i++ {
		newIndex := startingIndex + i
		// Create pods for role
		err := c.CreatePodsByRole(ctx, *targetRole.DeepCopy(), ms, newIndex, servingGroupOrdinal, newRevision)
		if err != nil {
			klog.Errorf("create role %s for ServingGroup %s failed: %v", utils.GenerateRoleID(targetRole.Name, newIndex), groupName, err)
		} else {
			// Insert new Role to global storage
			c.store.AddRole(utils.GetNamespaceName(ms), groupName, targetRole.Name, utils.GenerateRoleID(targetRole.Name, newIndex), newRevision)
		}
	}
}

// manageRoleReplicas manages the replicas of a specific role within an Serving group
// It handles both scale up and scale down operations for the role
func (c *ModelServingController) manageRoleReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, servingGroupOrdinal int, newRevision string) {
	// TODO: add podGroup update after gang scheduler finished
	// Get all replicas of a role from storage, for example, prefill-0, prefill-1...
	roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, targetRole.Name)
	if err != nil {
		klog.Errorf("cannot get role %s in ServingGroup %s, err:%v", targetRole.Name, groupName, err)
		return
	}

	expectedCount := int(*targetRole.Replicas)
	if len(roleList) == expectedCount {
		klog.V(4).Infof("The replicas of role %s in ServingGroup %s is consistent, no need to scale up or down", targetRole.Name, groupName)
		return
	}

	// Determsne whether it is a scale-up or scale-down scenario
	if len(roleList) < expectedCount {
		// Handle scale up by calling scaleUpRoles
		c.scaleUpRoles(ctx, ms, groupName, targetRole, roleList, expectedCount, servingGroupOrdinal, newRevision)
	} else {
		// Handle scale down by calling scaleDownRoles
		c.scaleDownRoles(ctx, ms, groupName, targetRole, roleList, expectedCount)
	}
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

func (c *ModelServingController) DeleteRole(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) {
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.GroupNameLabelKey: groupName,
		workloadv1alpha1.RoleLabelKey:      roleName,
		workloadv1alpha1.RoleIDKey:         roleID,
	})
	// If the role is already in the deletion process, no further processing will be done.
	roleStatus := c.store.GetRoleStatus(utils.GetNamespaceName(ms), groupName, roleName, roleID)
	if roleStatus == datastore.RoleDeleting {
		return
	}
	err := c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, roleName, roleID, datastore.RoleDeleting)
	if err != nil {
		klog.Errorf("failed to set role %s/%s status: %v", groupName, roleID, err)
		return
	}
	// Delete all pods in role
	err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).DeleteCollection(
		ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{
			LabelSelector: selector.String(),
		},
	)
	if err != nil {
		klog.Errorf("failed to delete pods of role %s/%s: %v", groupName, roleID, err)
	}
	// There is no DeleteCollection operation in the service of client-go. We need to list and delete them one by one.
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID)
	services, err := c.getServicesByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		klog.Errorf("failed to get service %v", err)
		return
	}
	for _, svc := range services {
		err = c.kubeClientSet.CoreV1().Services(ms.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("service %s/%s has been deleted", ms.Namespace, svc.Name)
			}
			klog.Errorf("failed to delete service %s/%s: %v", ms.Namespace, svc.Name, err)
		}
	}

	// Ensure that the role of an individual pod can be successfully enqueue.
	if c.isRoleDeleted(ms, groupName, roleName, roleID) {
		// role has been deleted, so the storage needs to be updated and need to reconcile.
		klog.V(2).Infof("role %s of servingGroup %s has been deleted", roleID, groupName)
		c.store.DeleteRole(utils.GetNamespaceName(ms), groupName, roleName, roleID)
		c.enqueueModelServing(ms)
	}
}

func (c *ModelServingController) manageServingGroupRollingUpdate(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision string) error {
	maxUnavailable, err := utils.GetMaxUnavailable(ms)
	if err != nil {
		return fmt.Errorf("failed to calculate maxUnavailable: %v", err)
	}

	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		return fmt.Errorf("cannot get ServingGroupList from store, err:%v", err)
	}

	// Count unavailable groups and filter outdated groups
	// Following Kubernetes deployment controller's behavior: delete unavailable groups first
	partition := c.getPartition(ms)
	currentUnavailableCount := 0
	var outdatedGroups []ServingGroupWithScore
	var unavailableOutdatedGroups []ServingGroupWithScore
	var availableOutdatedGroups []ServingGroupWithScore

	for _, sg := range servingGroupList {
		if sg.Status != datastore.ServingGroupRunning {
			currentUnavailableCount++
		}

		// Filter outdated groups and calculate scores for priority-based deletion
		_, ordinal := utils.GetParentNameAndOrdinal(sg.Name)
		if (partition == 0 || ordinal >= partition) && c.isServingGroupOutdated(sg, ms.Namespace, revision) {
			score := c.calculateServingGroupScore(ms, sg.Name)
			outdatedGroups = append(outdatedGroups, score)
			// Separate unavailable and available outdated groups
			if sg.Status != datastore.ServingGroupRunning {
				unavailableOutdatedGroups = append(unavailableOutdatedGroups, score)
			} else {
				availableOutdatedGroups = append(availableOutdatedGroups, score)
			}
		}
	}

	if len(outdatedGroups) == 0 {
		klog.V(4).Infof("no outdated groups to update for modelServing %s", ms.Name)
		return nil
	}

	// Sort outdated groups by priority: unavailable groups first, then by deletion cost and index
	slices.SortFunc(unavailableOutdatedGroups, compareServingGroupScore)
	slices.SortFunc(availableOutdatedGroups, compareServingGroupScore)

	// Delete unavailable outdated groups first (they don't increase unavailable count)
	// Then delete available outdated groups respecting maxUnavailable constraint
	updateCount := 0

	// First, delete all unavailable outdated groups (they're already unavailable, so deleting them is safe)
	for i := 0; i < len(unavailableOutdatedGroups); i++ {
		targetGroup := unavailableOutdatedGroups[i]
		klog.V(2).Infof("ServingGroup %s will be terminated for update (priority: %d, deletion cost: %d, index: %d, partition=%d) - unavailable group",
			targetGroup.Name, targetGroup.Priority, targetGroup.DeletionCost, targetGroup.Index, partition)
		if err := c.deleteServingGroup(ctx, ms, targetGroup.Name); err != nil {
			return err
		}
		updateCount++
	}

	// Then, delete available outdated groups respecting maxUnavailable constraint
	// After deleting unavailable groups, we can delete up to (maxUnavailable - remaining unavailable count) available groups
	remainingUnavailableCount := currentUnavailableCount - len(unavailableOutdatedGroups)
	groupToDelete := maxUnavailable - remainingUnavailableCount
	if groupToDelete > 0 {
		availableToDelete := min(len(availableOutdatedGroups), groupToDelete)
		for i := 0; i < availableToDelete; i++ {
			targetGroup := availableOutdatedGroups[i]
			klog.V(2).Infof("ServingGroup %s will be terminated for update (priority: %d, deletion cost: %d, index: %d, partition=%d) - available group",
				targetGroup.Name, targetGroup.Priority, targetGroup.DeletionCost, targetGroup.Index, partition)
			if err := c.deleteServingGroup(ctx, ms, targetGroup.Name); err != nil {
				return err
			}
			updateCount++
		}
	} else if len(availableOutdatedGroups) > 0 {
		klog.V(4).Infof("cannot delete available outdated groups: remaining unavailable count %d would exceed maxUnavailable %d", remainingUnavailableCount, maxUnavailable)
	}

	if updateCount > 0 {
		klog.V(2).Infof("terminated %d outdated groups for modelServing %s (partition=%d)", updateCount, ms.Name, partition)
	}

	return nil
}

func (c *ModelServingController) handleReadyPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, newPod *corev1.Pod) error {
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return fmt.Errorf("build plugin chain: %w", err)
	}
	if chain != nil {
		if err := chain.OnPodReady(context.Background(), &plugins.HookRequest{
			ModelServing: ms,
			ServingGroup: servingGroupName,
			RoleName:     utils.GetRoleName(newPod),
			RoleID:       utils.GetRoleID(newPod),
			IsEntry:      newPod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry,
			Pod:          newPod,
		}); err != nil {
			return err
		}
	}

	// Add the running pod to the global storage and try to update the ServingGroup status
	c.store.AddRunningPodToServingGroup(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	}, servingGroupName, newPod.Name, utils.PodRevision(newPod), utils.GetRoleName(newPod), utils.GetRoleID(newPod))
	ready, err := c.checkServingGroupReady(ms, servingGroupName)
	if err != nil {
		return fmt.Errorf("failed to check ServingGroup status, err: %v", err)
	}
	if ready {
		// All pods in the ServingGroup are running, so the ServingGroup status also needs to be set to running
		err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupRunning)
		if err != nil {
			return fmt.Errorf("failed to set ServingGroup %s status: %v", servingGroupName, err)
		}
		klog.V(2).Infof("Update ServingGroup %s status to Running", servingGroupName)
		c.enqueueModelServing(ms)
	} else {
		klog.V(4).Infof("ServingGroup %s still creating", servingGroupName)
	}
	return nil
}

func (c *ModelServingController) handleErrorPod(ms *workloadv1alpha1.ModelServing, servingGroupName string, errPod *corev1.Pod) error {
	// pod is already in the grace period and does not need to be processed for the time being.
	_, exists := c.graceMap.Load(utils.GetNamespaceName(errPod))
	now := time.Now()
	if exists {
		klog.V(4).Infof("Pod %v failed, waiting for grace time", utils.GetNamespaceName(errPod))
		return nil
	}
	// add pod to the grace period map
	c.graceMap.Store(utils.GetNamespaceName(errPod), now)
	c.store.DeleteRunningPodFromServingGroup(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	}, servingGroupName, errPod.Name)

	// Update role status back to Creating when pod fails
	roleName := utils.GetRoleName(errPod)
	roleID := utils.GetRoleID(errPod)
	if roleStatus := c.store.GetRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID); roleStatus == datastore.RoleRunning {
		err := c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID, datastore.RoleCreating)
		if err != nil {
			klog.Warningf("failed to update role %s/%s status to Creating: %v", roleName, roleID, err)
		} else {
			klog.V(2).Infof("update role %s/%s to Creating when pod fails", roleName, roleID)
		}
	}

	// If the ServingGroup status is already running, the status needs to be updated
	if groupStatus := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName); groupStatus == datastore.ServingGroupRunning {
		err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupCreating)
		if err != nil {
			return fmt.Errorf("update ServingGroup status failed, err:%v", err)
		}
		klog.V(2).Infof("update ServingGroup %s to processing when pod fails", servingGroupName)
	}
	// Wait for the grace period before processing
	go c.handlePodAfterGraceTime(ms, errPod)
	// ServingGroup status may change, needs reconcile
	c.enqueueModelServing(ms)
	return nil
}

func (c *ModelServingController) handlePodAfterGraceTime(ms *workloadv1alpha1.ModelServing, errPod *corev1.Pod) {
	if ms.Spec.Template.RestartGracePeriodSeconds != nil && *ms.Spec.Template.RestartGracePeriodSeconds > 0 {
		// Wait for the grace period before making a decision
		time.Sleep(time.Duration(*ms.Spec.Template.RestartGracePeriodSeconds) * time.Second)
		klog.V(4).Infof("%s after grace time", errPod.Name)
		defer c.graceMap.Delete(utils.GetNamespaceName(errPod))

		newPod, err := c.podsLister.Pods(ms.Namespace).Get(errPod.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("pod %s has been deleted after grace time", errPod.Name)
			} else {
				klog.Errorf("cannot get pod %s after grace time, err: %v", errPod.Name, err)
			}
			return
		}

		if !utils.IsPodRunningAndReady(newPod) {
			// pod has not recovered after the grace period, needs to be rebuilt
			// After this pod has been deleted, we will rebuild the ServingGroup in deletePod function
			err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(context.TODO(), newPod.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("cannot delete pod %s after grace time, err: %v", newPod.Name, err)
				return
			}
			klog.V(2).Infof("%s been deleted after grace time", errPod.Name)
		}
	} else {
		// grace period is not set or the grace period is 0, the deletion will be executed immediately.
		defer c.graceMap.Delete(utils.GetNamespaceName(errPod))

		err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(context.TODO(), errPod.Name, metav1.DeleteOptions{})
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
			if err != nil {
				return fmt.Errorf("failed to set ServingGroup %s status: %v", servingGroupName, err)
			}
		}
		c.DeleteRole(context.Background(), ms, servingGroupName, utils.GetRoleName(pod), utils.GetRoleID(pod))
	}
	return nil
}

func (c *ModelServingController) checkServingGroupReady(ms *workloadv1alpha1.ModelServing, servingGroupName string) (bool, error) {
	// TODO: modify ServingGroupReady logic after rolling update functionality is implemented
	runningPodsNum, err := c.store.GetRunningPodNumByServingGroup(utils.GetNamespaceName(ms), servingGroupName)
	if err != nil {
		return false, err
	}
	if runningPodsNum != utils.ExpectedPodNum(ms) {
		// the number of running pods does not reach the expected number
		return false, nil
	}
	return true, nil
}

func (c *ModelServingController) checkRoleReady(ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) (bool, error) {
	// Get all pods for this specific role
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, servingGroupName, roleName, roleID)
	pods, err := c.getPodsByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		return false, fmt.Errorf("failed to get pods for role %s/%s: %v", roleName, roleID, err)
	}

	// Find the role specification to get expected pod count
	var targetRole *workloadv1alpha1.Role
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			targetRole = &ms.Spec.Template.Roles[i]
			break
		}
	}

	if targetRole == nil {
		klog.Warningf("role %s not found in ModelServing spec", roleName)
		return false, nil
	}

	// Calculate expected pod count for this role replica
	// Each role replica has 1 entry pod + workerReplicas worker pods
	expectedPods := 1 + int(targetRole.WorkerReplicas)

	// Count running and ready pods
	runningPods := 0
	for _, pod := range pods {
		if utils.IsPodRunningAndReady(pod) {
			runningPods++
		}
	}

	if runningPods != expectedPods {
		// the number of running pods does not reach the expected number
		klog.V(4).Infof("Role %s/%s: %d/%d pods running", roleName, roleID, runningPods, expectedPods)
		return false, nil
	}

	klog.V(4).Infof("Role %s/%s: all %d pods are running", roleName, roleID, runningPods)
	return true, nil
}

func (c *ModelServingController) isServingGroupOutdated(group datastore.ServingGroup, namespace, newRevision string) bool {
	// Find the pods corresponding to ServingGroup
	groupNameValue := fmt.Sprintf("%s/%s", namespace, group.Name)
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("cannot list pod when check group updated,err: %v", err)
		return true
	}
	// Check all pods match the newHash
	for _, pod := range pods {
		if utils.PodRevision(pod) != newRevision {
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

// shouldSkipPodHandling checks if a pod should be skipped based on revision mismatch
func (c *ModelServingController) shouldSkipPodHandling(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) bool {
	podRevision := utils.PodRevision(pod)
	servingGroup := c.store.GetServingGroup(types.NamespacedName{
		Namespace: ms.Namespace,
		Name:      ms.Name,
	}, servingGroupName)
	if servingGroup != nil && servingGroup.Revision != podRevision {
		// If the pod revision is not equal to the ServingGroup revision, we do not need to handle it.
		klog.V(4).Infof("pod %s/%s revision %s is not equal to ServingGroup %s revision %s, skip handling",
			pod.Namespace, pod.Name, podRevision, servingGroupName, servingGroup.Revision)
		return true
	}
	return false
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
	// check ServingGroup status
	if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName) == datastore.ServingGroupDeleting {
		// ServingGroup is already in the deletion process, only checking whether the deletion is completed
		if c.isServingGroupDeleted(ms, servingGroupName) {
			// ServingGroup has been deleted, so the storage needs to be updated and need to reconcile.
			klog.V(2).Infof("servingGroup %s has been deleted", servingGroupName)
			c.store.DeleteServingGroup(utils.GetNamespaceName(ms), servingGroupName)
			c.enqueueModelServing(ms)
		}
		return true
	}

	// check role status
	if c.store.GetRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID) == datastore.RoleDeleting {
		// role is already in the deletion process, only checking whether the deletion is completed
		if c.isRoleDeleted(ms, servingGroupName, roleName, roleID) {
			// role has been deleted, so the storage needs to be updated and need to reconcile.
			klog.V(2).Infof("role %s of servingGroup %s has been deleted", roleID, servingGroupName)
			c.store.DeleteRole(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID)
			c.enqueueModelServing(ms)
		}
		return true
	}

	return false
}

func (c *ModelServingController) isServingGroupDeleted(ms *workloadv1alpha1.ModelServing, servingGroupName string) bool {
	status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName)
	if status != datastore.ServingGroupDeleting {
		// It will be determsned whether all resource have been deleted only when the group status is deleting.
		return false
	}
	// check whether the ServingGroup deletion has been completed
	groupNameValue := fmt.Sprintf("%s/%s", ms.Namespace, servingGroupName)
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("failed to get pod, err: %v", err)
		return false
	}
	services, err := c.getServicesByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("failed to get service, err:%v", err)
		return false
	}
	return len(pods) == 0 && len(services) == 0
}

func (c *ModelServingController) isRoleDeleted(ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) bool {
	if c.store.GetRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID) != datastore.RoleDeleting {
		// It will be determsned whether all resource have been deleted only when the role status is deleting.
		return false
	}
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, servingGroupName, roleName, roleID)
	// check whether the role deletion has been completed
	pods, err := c.getPodsByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		klog.Errorf("failed to get pod, err: %v", err)
		return false
	}
	services, err := c.getServicesByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		klog.Errorf("failed to get service, err:%v", err)
		return false
	}
	return len(pods) == 0 && len(services) == 0
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

// getServicesByIndex filter services using the informer indexer.
func (c *ModelServingController) getServicesByIndex(indexName, indexValue string) ([]*corev1.Service, error) {
	indexer := c.servicesInformer.GetIndexer()
	if _, exists := indexer.GetIndexers()[indexName]; !exists {
		return nil, fmt.Errorf("service indexer %s not found", indexName)
	}
	objs, err := indexer.ByIndex(indexName, indexValue)
	if err != nil {
		return nil, err
	}

	var services []*corev1.Service
	for _, obj := range objs {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			klog.Errorf("unexpected object type in service indexer: %T", obj)
			continue
		}
		services = append(services, svc)
	}
	return services, nil
}

// UpdateModelServingStatus update replicas in modelServing status.
func (c *ModelServingController) UpdateModelServingStatus(ms *workloadv1alpha1.ModelServing, revision string) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		// If no groups exist, handle gracefully by setting revisions to the new revision
		if errors.Is(err, datastore.ErrServingGroupNotFound) {
			copy := ms.DeepCopy()
			if copy.Status.CurrentRevision != revision || copy.Status.UpdateRevision != revision {
				copy.Status.CurrentRevision = revision
				copy.Status.UpdateRevision = revision
				_, updateErr := c.modelServingClient.WorkloadV1alpha1().ModelServings(copy.GetNamespace()).UpdateStatus(context.TODO(), copy, metav1.UpdateOptions{})
				return updateErr
			}
			return nil
		}
		return err
	}

	available, updated, current := 0, 0, 0
	progressingGroups, updatedGroups, currentGroups := []int{}, []int{}, []int{}
	// Track revision counts to determine the most common non-updated revision (CurrentRevision)
	revisionCount := make(map[string]int)
	for index := range groups {
		if groups[index].Status == datastore.ServingGroupDeleting {
			// Scaling -> Running or
			// Creating -> Running
			// No Deleting -> Running.
			// So directly add deleting groups to progressingGroups
			progressingGroups = append(progressingGroups, index)
			continue
		}

		if groups[index].Status == datastore.ServingGroupRunning {
			available = available + 1
		} else if ok, err := c.checkServingGroupReady(ms, groups[index].Name); ok && err == nil {
			// some scenarios, pod events may not trigger group status updates, such as role scaling down.
			err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groups[index].Name, datastore.ServingGroupRunning)
			if err != nil {
				return fmt.Errorf("failed to set servingGroup %s status: %v", groups[index].Name, err)
			}
			available = available + 1
			klog.V(2).Infof("Update servingGroup %s status to Running", groups[index].Name)
		} else {
			progressingGroups = append(progressingGroups, index)
		}

		if groups[index].Revision == revision {
			updated = updated + 1
			updatedGroups = append(updatedGroups, index)
		} else {
			current = current + 1
			currentGroups = append(currentGroups, index)
			// Count revisions for non-updated groups to find the most common one
			revisionCount[groups[index].Revision]++
		}
	}

	copy := ms.DeepCopy()
	shouldUpdate := utils.SetCondition(copy, progressingGroups, updatedGroups, currentGroups)
	if copy.Status.Replicas != int32(len(groups)) || copy.Status.AvailableReplicas != int32(available) || copy.Status.UpdatedReplicas != int32(updated) || copy.Status.CurrentReplicas != int32(current) {
		shouldUpdate = true
		copy.Status.Replicas = int32(len(groups))
		copy.Status.AvailableReplicas = int32(available)
		copy.Status.UpdatedReplicas = int32(updated)
		copy.Status.CurrentReplicas = int32(current)
	}

	// Update revision fields following StatefulSet's logic:
	// 1. UpdateRevision is always the new revision being applied
	// 2. CurrentRevision is read from Status.CurrentRevision if it exists and is still valid
	// 3. If Status.CurrentRevision doesn't exist or is invalid, compute from current groups
	// 4. When all groups are updated, CurrentRevision = UpdateRevision
	updateRevision := revision
	var currentRevision string

	// First, try to use existing CurrentRevision from status if it's still valid
	if copy.Status.CurrentRevision != "" {
		// Check if CurrentRevision is still valid (exists in non-updated groups)
		if len(revisionCount) > 0 {
			// Check if the existing CurrentRevision is still used by some groups
			if count, exists := revisionCount[copy.Status.CurrentRevision]; exists && count > 0 {
				currentRevision = copy.Status.CurrentRevision
			}
		}
		// If all groups are updated, CurrentRevision should equal UpdateRevision
		if updated == len(groups) {
			currentRevision = updateRevision
		}
	}

	// If CurrentRevision is not set (either not in status or invalid), compute it from current groups
	if currentRevision == "" {
		if updated == len(groups) || len(revisionCount) == 0 {
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

	if copy.Status.ObservedGeneration != ms.Generation {
		shouldUpdate = true
		copy.Status.ObservedGeneration = ms.Generation
	}

	if shouldUpdate {
		_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(copy.GetNamespace()).UpdateStatus(context.TODO(), copy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		// Clean up old revisions only after roles have been updated (revision status changed)
		if revisionUpdated {
			if cleanupErr := utils.CleanupOldControllerRevisions(context.TODO(), c.kubeClientSet, copy); cleanupErr != nil {
				klog.Warningf("Failed to cleanup old ControllerRevisions after updating revision status for ModelServing %s/%s: %v", copy.Namespace, copy.Name, cleanupErr)
			}
		}
	}

	return nil
}

// getPartition returns the partition value from ModelServing spec, or 0 if not set.
func (c *ModelServingController) getPartition(ms *workloadv1alpha1.ModelServing) int {
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.RollingUpdateConfiguration != nil && ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition != nil {
		return int(*ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition)
	}
	return 0
}

// scaleDownServingGroups scales down the ServingGroups to the expected count with two-level priority-based selection:
// 1. Primary: Not-ready groups (Creating, NotFound) are deleted first
// 2. Secondary: Among groups with same status, lower deletion cost = delete first
// When partition is set, the first N replicas (where N = partition) are protected.
// Non-protected replicas (after the first N) are deleted first, then protected replicas if needed.
func (c *ModelServingController) scaleDownServingGroups(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupList []datastore.ServingGroup, expectedCount int) error {
	partition := c.getPartition(ms)

	// Calculate scores for all servingGroups first
	allScores := make([]ServingGroupWithScore, 0, len(servingGroupList))
	for _, group := range servingGroupList {
		scoreInfo := c.calculateServingGroupScore(ms, group.Name)
		allScores = append(allScores, scoreInfo)
	}

	// Split scores by partition (servingGroupList is sorted by ordinal in ascending order)
	var protectedScores []ServingGroupWithScore
	var nonProtectedScores []ServingGroupWithScore

	if partition > 0 {
		splitIndex := min(partition, len(allScores))
		protectedScores = allScores[:splitIndex]
		nonProtectedScores = allScores[splitIndex:]
	} else {
		nonProtectedScores = allScores
	}

	// Sort both lists by priority tuple: (priority, deletionCost, index)
	slices.SortFunc(nonProtectedScores, compareServingGroupScore)

	totalToDelete := max(0, len(servingGroupList)-expectedCount)

	var err []error
	// Delete non-protected groups first (replicas after the first partition replicas)
	numNonProtectedToDelete := min(totalToDelete, len(nonProtectedScores))

	for i := 0; i < numNonProtectedToDelete; i++ {
		targetGroup := nonProtectedScores[i]
		klog.V(2).Infof("Scaling down non-protected serving group %s (priority: %d, deletion cost: %d, index: %d)",
			targetGroup.Name, targetGroup.Priority, targetGroup.DeletionCost, targetGroup.Index)
		if e := c.deleteServingGroup(ctx, ms, targetGroup.Name); e != nil {
			err = append(err, e)
		}
	}

	// After all non-protected groups are deleted, proceed to delete protected groups if needed
	remainingToDelete := totalToDelete - numNonProtectedToDelete
	if remainingToDelete > 0 && partition > 0 {
		// Sort protected scores only when we need to delete them
		slices.SortFunc(protectedScores, compareServingGroupScore)
		numProtectedToDelete := min(remainingToDelete, len(protectedScores))

		for i := 0; i < numProtectedToDelete; i++ {
			targetGroup := protectedScores[i]
			klog.V(2).Infof("Scaling down protected serving group %s (priority: %d, deletion cost: %d, index: %d, partition=%d)",
				targetGroup.Name, targetGroup.Priority, targetGroup.DeletionCost, targetGroup.Index, partition)
			// Note: ControllerRevision history recording for partition-protected groups is handled in deleteServingGroup
			if e := c.deleteServingGroup(ctx, ms, targetGroup.Name); e != nil {
				err = append(err, e)
			}
		}
	}

	if len(err) > 0 {
		return errors.Join(err...)
	}

	return nil
}

func (c *ModelServingController) manageHeadlessService(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	servingGroups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get servingGroups: %v", err)
	}

	for _, sg := range servingGroups {
		if sg.Status == datastore.ServingGroupDeleting {
			continue
		}
		for _, role := range ms.Spec.Template.Roles {
			roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), sg.Name, role.Name)
			if err != nil {
				continue
			}

			for _, roleObj := range roleList {
				if roleObj.Status == datastore.RoleDeleting {
					continue
				}

				serviceSelector := map[string]string{
					workloadv1alpha1.GroupNameLabelKey: sg.Name,
					workloadv1alpha1.RoleLabelKey:      role.Name,
					workloadv1alpha1.RoleIDKey:         roleObj.Name,
				}

				services, err := c.getServicesByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, sg.Name, role.Name, roleObj.Name))
				if err != nil {
					continue
				}

				if len(services) == 0 && role.WorkerTemplate != nil {
					_, roleIndex := utils.GetParentNameAndOrdinal(roleObj.Name)
					if err := utils.CreateHeadlessService(ctx, c.kubeClientSet, ms, serviceSelector, sg.Name, role.Name, roleIndex); err != nil {
						klog.Errorf("failed to create service for role %s in serving group %s: %v", roleObj.Name, sg.Name, err)
					}
				}
			}
		}
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

func (c *ModelServingController) CreatePodsForServingGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupIndex int, revision string, roles []workloadv1alpha1.Role) error {
	servingGroupName := utils.GenerateServingGroupName(ms.Name, servingGroupIndex)
	for _, role := range roles {
		replicas := int(*role.Replicas)
		for i := 0; i < replicas; i++ {
			if err := c.CreatePodsByRole(ctx, *role.DeepCopy(), ms, i, servingGroupIndex, revision); err != nil {
				return err
			}
			roleID := utils.GenerateRoleID(role.Name, i)
			c.store.AddRole(utils.GetNamespaceName(ms), servingGroupName, role.Name, roleID, revision)
		}
	}
	return nil
}

func (c *ModelServingController) CreatePodsByRole(ctx context.Context, role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, roleIndex int, servingGroupOrdinal int, revision string) error {
	servingGroupName := utils.GenerateServingGroupName(ms.Name, servingGroupOrdinal)
	// TODO(hzxuzhonghu): build the plugin chain only once per ModelServing
	// This is not critical now, so we leave it for future optimization.
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return fmt.Errorf("build plugin chain: %w", err)
	}
	roleID := utils.GenerateRoleID(role.Name, roleIndex)

	entryPod := utils.GenerateEntryPod(role, ms, servingGroupName, roleIndex, revision)
	taskName := c.podGroupManager.GenerateTaskName(role.Name, roleIndex)
	c.podGroupManager.AnnotatePodWithPodGroup(entryPod, ms, servingGroupName, taskName)
	if chain != nil {
		entryReq := &plugins.HookRequest{
			ModelServing: ms,
			ServingGroup: servingGroupName,
			RoleName:     role.Name,
			RoleID:       roleID,
			IsEntry:      true,
			Pod:          entryPod,
		}
		if err := chain.OnPodCreate(ctx, entryReq); err != nil {
			return fmt.Errorf("execute OnPodCreate failed for entry pod %s: %v", entryPod.Name, err)
		}
	}
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, entryPod, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create entry pod %s: %v", entryPod.Name, err)
	}

	for i := 1; i <= int(role.WorkerReplicas); i++ {
		workerPod := utils.GenerateWorkerPod(role, ms, entryPod, servingGroupName, roleIndex, i, revision)
		c.podGroupManager.AnnotatePodWithPodGroup(workerPod, ms, servingGroupName, taskName)
		if chain != nil {
			workerReq := &plugins.HookRequest{
				ModelServing: ms,
				ServingGroup: servingGroupName,
				RoleName:     role.Name,
				RoleID:       roleID,
				IsEntry:      false,
				Pod:          workerPod,
			}
			if err := chain.OnPodCreate(ctx, workerReq); err != nil {
				return fmt.Errorf("execute OnPodCreate failed for worker pod %s: %v", workerPod.Name, err)
			}
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, workerPod, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create worker pod %s: %v", workerPod.Name, err)
		}
	}
	return nil
}

func (c *ModelServingController) deleteServingGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) error {
	status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName)
	if status == datastore.ServingGroupNotFound {
		return nil
	}

	// Record revision history using ControllerRevision before deleting, especially important for partition-protected servingGroups
	// This ensures that when a partition-protected ServingGroup is deleted (e.g., due to failure),
	// it can be recreated with its previous revision, following StatefulSet's behavior.
	group := c.store.GetServingGroup(utils.GetNamespaceName(ms), servingGroupName)
	if group != nil {
		_, ordinal := utils.GetParentNameAndOrdinal(servingGroupName)
		partition := c.getPartition(ms)
		// Record revision history using ControllerRevision for partition-protected servingGroups
		if partition > 0 && ordinal < partition {
			// Create ControllerRevision to persist the revision history
			// Store the template roles data for this revision
			templateData := ms.Spec.Template.Roles
			_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, group.Revision, templateData)
			if err != nil {
				klog.Warningf("Failed to create ControllerRevision for ServingGroup %s (revision=%s): %v", servingGroupName, group.Revision, err)
				// Note: We don't fallback to in-memory storage as ControllerRevision is the source of truth
			} else {
				klog.V(2).Infof("Created ControllerRevision for partition-protected ServingGroup %s (revision=%s, partition=%d)", servingGroupName, group.Revision, partition)
			}
		}
	}

	if err := c.podGroupManager.DeletePodGroup(ctx, ms, servingGroupName); err != nil {
		return fmt.Errorf("failed to delete PodGroup for ServingGroup %s: %v", servingGroupName, err)
	}

	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.GroupNameLabelKey: servingGroupName,
	})
	err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to delete pods of ServingGroup %s: %v", servingGroupName, err)
	}

	// Delete services
	services, err := c.getServicesByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, servingGroupName))
	if err != nil {
		return fmt.Errorf("failed to get services for ServingGroup %s: %v", servingGroupName, err)
	}

	for _, svc := range services {
		err = c.kubeClientSet.CoreV1().Services(ms.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete service %s: %v", svc.Name, err)
		}
	}

	// update ServingGroup status to Deleting after deleting pods and services
	if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupDeleting); err != nil {
		klog.ErrorS(err, "Failed to update ServingGroup status", "namespace", ms.Namespace, "servingGroup", servingGroupName)
		return err
	}

	if c.isServingGroupDeleted(ms, servingGroupName) {
		klog.V(2).Infof("ServingGroup %s has been deleted", servingGroupName)
		c.store.DeleteServingGroup(utils.GetNamespaceName(ms), servingGroupName)
		// this is needed when a pod is deleted accidentally, and the ServingGroup is deleted completely
		// and the controller has no chance to supplement it.
		c.enqueueModelServing(ms)
	}
	return nil
}
