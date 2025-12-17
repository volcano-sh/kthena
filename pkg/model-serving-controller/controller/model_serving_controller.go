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
	"reflect"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/gangscheduling"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	GroupNameKey = "GroupName"
	RoleIDKey    = "RoleID"
)

type ModelServingController struct {
	kubeClientSet      kubernetes.Interface
	modelServingClient clientset.Interface

	syncHandler           func(ctx context.Context, miKey string) error
	gangManager           gangscheduling.Manager
	podsLister            listerv1.PodLister
	podsInformer          cache.SharedIndexInformer
	servicesLister        listerv1.ServiceLister
	servicesInformer      cache.SharedIndexInformer
	modelServingLister    listerv1alpha1.ModelServingLister
	modelServingsInformer cache.SharedIndexInformer

	// nolint
	workqueue   workqueue.RateLimitingInterface
	store       datastore.Store
	graceMap    sync.Map // key: errorPod.namespace/errorPod.name, value:time
	initialSync bool     // indicates whether the initial sync has been completed
}

func NewModelServingController(kubeClientSet kubernetes.Interface, modelServingClient clientset.Interface, volcanoClient volcano.Interface) (*ModelServingController, error) {
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
		gangManager:           gangscheduling.NewManager(kubeClientSet, volcanoClient, store),
		podsLister:            podsInformer.Lister(),
		podsInformer:          podsInformer.Informer(),
		servicesLister:        servicesInformer.Lister(),
		servicesInformer:      servicesInformer.Informer(),
		modelServingLister:    modelServingInformer.Lister(),
		modelServingsInformer: modelServingInformer.Informer(),
		// nolint
		workqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ModelServings"),
		store:     store,
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

	_, _ = c.podsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.addPod(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.updatePod(oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			c.deletePod(obj)
		},
	})

	c.syncHandler = c.syncModelServing

	return c, nil
}

func (c *ModelServingController) addModelServing(obj interface{}) {
	mi, ok := obj.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Error("failed to parse ModelServing type when addMI")
		return
	}
	klog.V(4).InfoS("Adding", "modelServing", klog.KObj(mi))
	c.enqueueModelServing(mi)
}

func (c *ModelServingController) updateModelServing(old, cur interface{}) {
	curMI, ok := cur.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Error("failed to parse ModelServing type when updateMI")
		return
	}
	oldMI, ok := old.(*workloadv1alpha1.ModelServing)
	if !ok {
		klog.Error("failed to parse ModelServing type when updateMI")
		return
	}

	if reflect.DeepEqual(oldMI.Spec, curMI.Spec) {
		// If the spec has not changed, we do not need to reconcile.
		klog.V(4).InfoS("Spec has not changed, skipping update", "modelServing", klog.KObj(curMI))
		return
	}

	// If network topology is removed, we need to clean up the PodGroups.
	// Because MinRoleReplicas is not allowed to be updated, so we do not need to check it here.
	if oldMI.Spec.Template.NetworkTopology != nil && curMI.Spec.Template.NetworkTopology == nil {
		if curMI.Spec.Template.GangPolicy.MinRoleReplicas == nil {
			if err := c.gangManager.CleanupPodGroups(context.TODO(), curMI); err != nil {
				klog.Errorf("failed to clean up PodGroups for ModelServing %s/%s: %v", curMI.Namespace, curMI.Name, err)
			}
		}
	}

	c.enqueueModelServing(curMI)
}

func (c *ModelServingController) deleteModelServing(obj interface{}) {
	mi, ok := obj.(*workloadv1alpha1.ModelServing)
	if !ok {
		// If the object is not a ModelServing, it might be a tombstone object.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("failed to parse ModelServing type when deleteMI %#v", obj)
			return
		}
		mi, ok = tombstone.Obj.(*workloadv1alpha1.ModelServing)
		if !ok {
			klog.Errorf("failed to parse ModelServing from tombstone %#v", tombstone.Obj)
			return
		}
	}

	c.store.DeleteModelServing(types.NamespacedName{
		Namespace: mi.Namespace,
		Name:      mi.Name,
	})
}

func (c *ModelServingController) addPod(obj interface{}) {
	c.updatePod(nil, obj)
}

func (c *ModelServingController) updatePod(oldObj, newObj interface{}) {
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

	mi, servingGroupName, err := c.getModelServing(newPod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("modelServing of pod %s has been deleted", newPod.Name)
		} else {
			klog.Errorf("get model Serving failed when update pod: %v", err)
		}
		return
	}

	if c.shouldSkipPodHandling(mi, servingGroupName, newPod) {
		// Pod revision mismatch ServingGroup, this can rarely happen
		return
	}

	switch {
	case utils.IsPodRunningAndReady(newPod):
		// The pod is available, that is, the state is running, and the container is ready
		err = c.handleReadyPod(mi, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle running pod failed: %v", err)
		}
	case utils.IsPodFailed(newPod) || utils.ContainerRestarted(newPod):
		// handleErrorPod is not called until modelServing has been called.
		if !c.initialSync {
			return
		}
		// Failure occurs in pod and we need to wait for a grace period before making a judgment.
		err = c.handleErrorPod(mi, servingGroupName, newPod)
		if err != nil {
			klog.Errorf("handle error pod failed: %v", err)
		}
	}
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

	mi, servingGroupName, err := c.getModelServing(pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("modelServing of pod %s/%s has been deleted", pod.GetNamespace(), pod.GetName())
		} else {
			klog.Errorf("failed to get modelServing of pod %s/%s: %v", pod.GetNamespace(), pod.GetName(), err)
		}
		return
	}

	roleName, roleID := utils.PodRoleName(pod), utils.PodRoleID(pod)
	// check ServingGroup status
	if c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName) == datastore.ServingGroupDeleting {
		// ServingGroup is already in the deletion process, only checking whether the deletion is completed
		if c.isServingGroupDeleted(mi, servingGroupName) {
			// ServingGroup has been deleted, so the storage needs to be updated and need to reconcile.
			klog.V(2).Infof("servingGroup %s has been deleted", servingGroupName)
			c.store.DeleteServingGroup(utils.GetNamespaceName(mi), servingGroupName)
			c.enqueueModelServing(mi)
		}
		return
	}

	c.store.DeleteRunningPodFromServingGroup(types.NamespacedName{
		Namespace: mi.Namespace,
		Name:      mi.Name,
	}, servingGroupName, pod.Name)

	// check role status
	if c.store.GetRoleStatus(utils.GetNamespaceName(mi), servingGroupName, roleName, roleID) == datastore.RoleDeleting {
		// role is already in the deletion process, only checking whether the deletion is completed
		if c.isRoleDeleted(mi, servingGroupName, utils.PodRoleName(pod), utils.PodRoleID(pod)) {
			// role has been deleted, so the storage needs to be updated and need to reconcile.
			klog.V(2).Infof("role %s of servingGroup %s has been deleted", utils.PodRoleID(pod), servingGroupName)
			c.store.DeleteRole(utils.GetNamespaceName(mi), servingGroupName, roleName, roleID)
			c.enqueueModelServing(mi)
		}
		return
	}

	if c.shouldSkipPodHandling(mi, servingGroupName, pod) {
		return
	}

	err = c.handleDeletedPod(mi, servingGroupName, pod)
	if err != nil {
		klog.Errorf("handle deleted pod failed: %v", err)
	}
}

func (c *ModelServingController) enqueueModelServing(mi *workloadv1alpha1.ModelServing) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(mi); err != nil {
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
	// TODO: Consider obtaining the pod status during the modelServing reconcile to process the Servinggroup status. This can ensure the real-time status.
	klog.V(4).Info("Started syncing ModelServing")
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", err)
	}

	mi, err := c.modelServingLister.ModelServings(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		klog.V(4).Infof("%v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}
	// only fields in roles can be modified in rolling updates.
	// and only modifying the role.replicas field will not affect the revision.
	copy := utils.RemoveRoleReplicasForRevision(mi)
	revision := utils.Revision(copy.Spec.Template.Roles)

	// PodGroup Manager
	if err := c.gangManager.ManagePodGroups(ctx, mi); err != nil {
		return fmt.Errorf("Failed to manage PodGroups for ModelServing %s/%s: %v", mi.Namespace, mi.Name, err)
	}

	err = c.manageServingGroupReplicas(ctx, mi, revision)
	if err != nil {
		return fmt.Errorf("cannot manage ServingGroup replicas: %v", err)
	}

	err = c.manageRole(ctx, mi, revision)
	if err != nil {
		return fmt.Errorf("cannot manage role replicas: %v", err)
	}

	err = c.manageServingGroupRollingUpdate(mi, revision)
	if err != nil {
		return fmt.Errorf("cannot manage ServingGroup rollingUpdate: %v", err)
	}

	if err := c.UpdateModelServingStatus(mi, revision); err != nil {
		return fmt.Errorf("failed to update status of mi %s/%s: %v", namespace, name, err)
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

// sync all pods before starting the worker
// we do not need to sync ModelServing here, because the ModelServing controller will sync all ModelServings after the initial sync.
// Related ServingGroups will be created when syncing pods.
func (c *ModelServingController) syncAll() {
	pods, _ := c.podsLister.List(labels.Everything())
	for _, pod := range pods {
		c.addPod(pod)
	}

	c.initialSync = true
}

// UpdateModelServingConditionsStatus update conditions ModelServing status.
func (c *ModelServingController) UpdateModelServingConditionsStatus(mi *workloadv1alpha1.ModelServing, condition metav1.Condition) error {
	if !meta.SetStatusCondition(&mi.Status.Conditions, condition) {
		return fmt.Errorf("failed to update modelServing %s/%s status conditions", mi.GetNamespace(), mi.GetName())
	}
	return nil
}

// UpdateModelServingStatus update replicas in modelServing status.
func (c *ModelServingController) UpdateModelServingStatus(mi *workloadv1alpha1.ModelServing, revision string) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
	if err != nil {
		return err
	}

	available, updated, current := 0, 0, 0
	progressingGroups, updatedGroups, currentGroups := []int{}, []int{}, []int{}
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
		} else if ok, err := c.checkServingGroupReady(mi, groups[index].Name); ok && err == nil {
			// some scenarios, pod events may not trigger group status updates, such as role scaling down.
			err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), groups[index].Name, datastore.ServingGroupRunning)
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
		}
	}

	copy := mi.DeepCopy()
	shouldUpdate := utils.SetCondition(copy, progressingGroups, updatedGroups, currentGroups)
	if copy.Status.Replicas != int32(len(groups)) || copy.Status.AvailableReplicas != int32(available) || copy.Status.UpdatedReplicas != int32(updated) || copy.Status.CurrentReplicas != int32(current) {
		shouldUpdate = true
		copy.Status.Replicas = int32(len(groups))
		copy.Status.AvailableReplicas = int32(available)
		copy.Status.UpdatedReplicas = int32(updated)
		copy.Status.CurrentReplicas = int32(current)
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

	if copy.Status.ObservedGeneration != mi.Generation {
		shouldUpdate = true
		copy.Status.ObservedGeneration = mi.Generation
	}

	if shouldUpdate {
		_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(copy.GetNamespace()).UpdateStatus(context.TODO(), copy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *ModelServingController) manageServingGroupReplicas(ctx context.Context, mi *workloadv1alpha1.ModelServing, newRevision string) error {
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get servingGroup of modelServing: %s from map: %v", mi.GetName(), err)
	}
	expectedCount := int(*mi.Spec.Replicas)
	curReplicas := len(servingGroupList)
	if curReplicas == expectedCount {
		klog.V(4).Info("The number of replicas is consistent, no need to scale up or down")
		return nil
	}

	// Determine whether it is a scale-up or scale-down scenario
	if curReplicas < expectedCount {
		err = c.scaleUpServingGroups(ctx, mi, servingGroupList, expectedCount, newRevision)
		if err != nil {
			return fmt.Errorf("failed to scale up ServingGroups: %v", err)
		}
	} else {
		err = c.scaleDownServingGroups(ctx, mi, servingGroupList, expectedCount)
		if err != nil {
			return fmt.Errorf("failed to scale down ServingGroups: %v", err)
		}
	}
	return nil
}

// scaleUpServingGroups scales up the ServingGroups to the expected count.
// It creates new ServingGroups with increasing indices starting from the current max index + 1.
func (c *ModelServingController) scaleUpServingGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing, servingGroupList []datastore.ServingGroup, expectedCount int, newRevision string) error {
	// Find the maximum existing valid index to determine the starting point for new ServingGroups
	// Also count only ServingGroups with valid ordinals
	maxIndex := -1
	validCount := 0
	for _, group := range servingGroupList {
		_, servingGroupOrdinal := utils.GetParentNameAndOrdinal(group.Name)
		if servingGroupOrdinal >= 0 {
			validCount++
			if servingGroupOrdinal > maxIndex {
				maxIndex = servingGroupOrdinal
			}
		}
	}

	// Calculate how many new ServingGroups we need to create
	toCreate := expectedCount - validCount
	if toCreate <= 0 {
		// No new ServingGroups need to be created
		return nil
	}

	// Create new ServingGroups with increasing indices
	for i := 0; i < toCreate; i++ {
		newIndex := maxIndex + 1 + i
		// Create pods for ServingGroup
		err := c.CreatePodsForServingGroup(ctx, mi, newIndex, newRevision)
		if err != nil {
			return fmt.Errorf("create Serving group failed: %v", err)
		}
		// Insert new ServingGroup to global storage
		c.store.AddServingGroup(utils.GetNamespaceName(mi), newIndex, newRevision)
	}

	return nil
}

// scaleDownServingGroups scales down the ServingGroups to the expected count.
func (c *ModelServingController) scaleDownServingGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing, servingGroupList []datastore.ServingGroup, expectedCount int) error {
	// Calculate scores for all ServingGroups
	servingGroupScores := make([]ServingGroupWithScore, 0, len(servingGroupList))

	needsSort := false
	for _, group := range servingGroupList {
		score, err := c.calculateServingGroupScore(mi, group.Name)
		if err != nil {
			klog.Errorf("Failed to calculate score for ServingGroup %s: %v", group.Name, err)
			continue
		}
		_, groupIndex := utils.GetParentNameAndOrdinal(group.Name)
		servingGroupScores = append(servingGroupScores, ServingGroupWithScore{
			Name:  group.Name,
			Score: score,
			Index: groupIndex,
		})
		if score != 0 {
			needsSort = true
		}
	}

	// Sort ServingGroups by score in descending order, and by index in ascending order if scores are equal.
	// This puts items with lower scores (or higher indices when scores are equal) at the end.
	// Skip sorting when all scores are 0 because servingGroupList is already sorted by index in ascending order.
	if needsSort {
		slices.SortFunc(servingGroupScores, func(a, b ServingGroupWithScore) int {
			if a.Score != b.Score {
				return cmp.Compare(b.Score, a.Score)
			}
			return cmp.Compare(a.Index, b.Index)
		})
	}

	var allErrors []error
	// Delete items from end to front. Items at the end have lower scores (or higher indices when scores are 0).
	for i := len(servingGroupScores) - 1; i >= expectedCount; i-- {
		targetName := servingGroupScores[i].Name
		c.DeleteServingGroup(mi, targetName)
		if err := c.gangManager.DeletePodGroupWhenServingGroupDeleted(ctx, mi, targetName); err != nil {
			allErrors = append(allErrors, err)
		}
	}

	if len(allErrors) > 0 {
		return fmt.Errorf("Delete ServingGroup failed: %v", allErrors)
	}

	return nil
}

func (c *ModelServingController) CreatePodsForServingGroup(ctx context.Context, mi *workloadv1alpha1.ModelServing, groupIndex int, newHash string) error {
	// traverse each role in ServingGroup to create entry-worker pod group.
	roleList := mi.Spec.Template.Roles
	for _, role := range roleList {
		// there will be multiple replicas in a role, such as xPyD type
		for roleIndex := range int(*role.Replicas) {
			err := c.CreatePodByRole(ctx, *role.DeepCopy(), mi, roleIndex, groupIndex, newHash)
			if err != nil {
				return fmt.Errorf("create role pod failed: %v, role name: %s, role index: %d", err, role.Name, roleIndex)
			}
		}
	}
	return nil
}

func (c *ModelServingController) DeleteServingGroup(mi *workloadv1alpha1.ModelServing, groupname string) {
	miNamedName := utils.GetNamespaceName(mi)
	servingGroupStatus := c.store.GetServingGroupStatus(miNamedName, groupname)
	if servingGroupStatus == datastore.ServingGroupNotFound {
		return
	}

	groupNameValue := fmt.Sprintf("%s/%s", miNamedName.Namespace, groupname)
	label := fmt.Sprintf("%s=%s", workloadv1alpha1.GroupNameLabelKey, groupname)
	if servingGroupStatus != datastore.ServingGroupDeleting {
		err := c.store.UpdateServingGroupStatus(miNamedName, groupname, datastore.ServingGroupDeleting)
		if err != nil {
			klog.Errorf("failed to set ServingGroup %s/%s status: %v", miNamedName.Namespace+"/"+mi.Name, groupname, err)
			return
		}
		// Delete all pods in ServingGroup
		err = c.kubeClientSet.CoreV1().Pods(miNamedName.Namespace).DeleteCollection(
			context.TODO(),
			metav1.DeleteOptions{},
			metav1.ListOptions{
				LabelSelector: label,
			},
		)
		if err != nil {
			klog.Errorf("failed to delete ServingGroup %s/%s: %v", miNamedName.Namespace+"/"+mi.Name, groupname, err)
			return
		}
		// There is no DeleteCollection operation in the service of client-go. We need to list and delete them one by one.
		services, err := c.getServicesByIndex(GroupNameKey, groupNameValue)
		if err != nil {
			klog.Errorf("failed to get service %v", err)
			return
		}
		for _, svc := range services {
			err = c.kubeClientSet.CoreV1().Services(miNamedName.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.V(4).Infof("service %s/%s has been deleted", miNamedName.Namespace, svc.Name)
				} else {
					klog.Errorf("failed to delete service %s/%s: %v", miNamedName.Namespace, svc.Name, err)
					return
				}
			}
		}
	}

	// check whether the deletion has been completed
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("failed to get pod, err:%v", err)
	}
	services, err := c.getServicesByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		klog.Errorf("failed to get service, err:%v", err)
	}
	if len(pods) == 0 && len(services) == 0 {
		klog.V(2).Infof("ServingGroup %s has been deleted", groupname)
		c.store.DeleteServingGroup(miNamedName, groupname)
		c.enqueueModelServing(mi)
		return
	}
}

func (c *ModelServingController) CreatePodByRole(ctx context.Context, role workloadv1alpha1.Role, mi *workloadv1alpha1.ModelServing, roleIndex, groupIndex int, newHash string) error {
	groupName := utils.GenerateServingGroupName(mi.Name, groupIndex)
	taskName := c.gangManager.GenerateTaskName(role.Name, roleIndex)
	// Create entry pod
	entryPod := utils.GenerateEntryPod(role, mi, groupName, roleIndex, newHash)

	c.gangManager.AnnotatePodWithPodGroup(entryPod, mi, 1+int(role.WorkerReplicas), groupName, taskName)

	_, err := c.kubeClientSet.CoreV1().Pods(mi.Namespace).Create(ctx, entryPod, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			klog.Errorf("create entry pod failed: %v", err)
			return err
		}
	}

	// Determine whether to create worker pods and headless service
	if role.WorkerTemplate == nil {
		klog.V(4).Info("workerTemplate is nil, no need to create worker pods and headless service")
		return nil
	}
	// Create headless service
	err = utils.CreateHeadlessService(ctx, c.kubeClientSet, mi, entryPod.ObjectMeta.Labels, groupName, role.Name, roleIndex)
	if err != nil {
		klog.Errorf("create headless service failed: %v", err)
		return err
	}
	// Create worker pods
	for podIndex := range int(role.WorkerReplicas) {
		workerPod := utils.GenerateWorkerPod(role, mi, entryPod, groupName, roleIndex, podIndex+1, newHash) // worker-pod sequence number starts from 1, so we use index+1 here.
		c.gangManager.AnnotatePodWithPodGroup(workerPod, mi, 1+int(role.WorkerReplicas), groupName, taskName)
		_, err = c.kubeClientSet.CoreV1().Pods(mi.Namespace).Create(ctx, workerPod, metav1.CreateOptions{})
		if err != nil {
			if !apierrors.IsAlreadyExists(err) {
				klog.Errorf("create worker pod failed: %v", err)
				return err
			}
		}
	}
	return nil
}

func (c *ModelServingController) manageRole(ctx context.Context, mi *workloadv1alpha1.ModelServing, newRevision string) error {
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return fmt.Errorf("cannot get ServingGroup of modelServing: %s from map: %v", mi.GetName(), err)
	}
	for _, servingGroup := range servingGroupList {
		if c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), servingGroup.Name) == datastore.ServingGroupDeleting {
			// Deleting ServingGroup will be recreated after the deletion is complete, so there is no need to scale the roles
			continue
		}
		_, servingGroupOrdinal := utils.GetParentNameAndOrdinal(servingGroup.Name)
		for _, targetRole := range mi.Spec.Template.Roles {
			c.manageRoleReplicas(ctx, mi, servingGroup.Name, targetRole, servingGroupOrdinal, newRevision)
		}
	}
	return nil
}

// scaleDownRoles handles Role scaling down
func (c *ModelServingController) scaleDownRoles(ctx context.Context, mi *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, roleList []datastore.Role, expectedCount int) {
	// Calculate scores for all Roles
	var roleScores []RoleWithScore

	needsSort := false
	for _, role := range roleList {
		// Get score for each Role.
		// If not set 'PodDeletionCost` annotation, the score is set to 0.
		score, err := c.calculateRoleScore(mi, groupName, targetRole.Name, role.Name)
		if err != nil {
			klog.Errorf("Failed to calculate score for role %s: %v", role.Name, err)
			continue
		}
		_, roleIndex := utils.GetParentNameAndOrdinal(role.Name)
		roleScores = append(roleScores, RoleWithScore{
			Name:  role.Name,
			Score: score,
			Index: roleIndex,
		})
		if score != 0 {
			needsSort = true
		}
	}

	// Sort Roles by score in descending order, and by index in ascending order if scores are equal.
	// This puts items with lower scores (or higher indices when scores are equal) at the end.
	// Skip sorting when all scores are 0 because roleList is already sorted by index in ascending order.
	if needsSort {
		slices.SortFunc(roleScores, func(a, b RoleWithScore) int {
			if a.Score != b.Score {
				return cmp.Compare(b.Score, a.Score)
			}
			return cmp.Compare(a.Index, b.Index)
		})
	}
	// Role needs to scale down, and the ServingGroup status needs to be set to Scaling
	if c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), groupName) != datastore.ServingGroupScaling {
		err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), groupName, datastore.ServingGroupScaling)
		if err != nil {
			klog.Errorf("failed to set ServingGroup %s/%s status: %v", mi.Namespace+"/"+mi.Name, groupName, err)
			return
		}
	}

	// Delete items from end to front. Items at the end have lower scores (or higher indices when scores are 0).
	for i := len(roleScores) - 1; i >= expectedCount; i-- {
		targetName := roleScores[i].Name
		c.DeleteRole(ctx, mi, groupName, targetRole.Name, targetName)
	}
}

// scaleUpRoles handles Role scaling up.
// It creates new Roles with increasing indices starting from the current max index + 1.
func (c *ModelServingController) scaleUpRoles(ctx context.Context, mi *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, roleList []datastore.Role, expectedCount int, servingGroupOrdinal int, newRevision string) {
	// Find the maximum existing valid index to determine the starting point for new Roles
	// Also count only Roles with valid ordinals
	maxIndex := -1
	validCount := 0
	for _, role := range roleList {
		_, roleOrdinal := utils.GetParentNameAndOrdinal(role.Name)
		if roleOrdinal >= 0 {
			validCount++
			if roleOrdinal > maxIndex {
				maxIndex = roleOrdinal
			}
		}
	}

	// Calculate how many new Roles we need to create
	toCreate := expectedCount - validCount
	if toCreate <= 0 {
		// No new Roles need to be created
		return
	}

	// Role needs to scale up, and the ServingGroup status needs to be set to Scaling
	if c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), groupName) != datastore.ServingGroupScaling {
		err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), groupName, datastore.ServingGroupScaling)
		if err != nil {
			klog.Errorf("failed to set ServingGroup %s/%s status: %v", mi.Namespace+"/"+mi.Name, groupName, err)
			return
		}
	}

	// Create new Roles with increasing indices
	for i := 0; i < toCreate; i++ {
		newIndex := maxIndex + 1 + i
		// Create pods for role
		err := c.CreatePodByRole(ctx, *targetRole.DeepCopy(), mi, newIndex, servingGroupOrdinal, newRevision)
		if err != nil {
			klog.Errorf("create role %s for ServingGroup %s failed: %v", utils.GenerateRoleID(targetRole.Name, newIndex), groupName, err)
		} else {
			// Insert new Role to global storage
			c.store.AddRole(utils.GetNamespaceName(mi), groupName, targetRole.Name, utils.GenerateRoleID(targetRole.Name, newIndex), newRevision)
		}
	}
}

// manageRoleReplicas manages the replicas of a specific role within an Serving group
// It handles both scale up and scale down operations for the role
func (c *ModelServingController) manageRoleReplicas(ctx context.Context, mi *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, servingGroupOrdinal int, newRevision string) {
	// TODO: add podGroup update after gang scheduler finished
	// Get all replicas of a role from storage, for example, prefill-0, prefill-1...
	roleList, err := c.store.GetRoleList(utils.GetNamespaceName(mi), groupName, targetRole.Name)
	if err != nil {
		klog.Errorf("cannot get role %s in ServingGroup %s, err:%v", targetRole.Name, groupName, err)
		return
	}

	expectedCount := int(*targetRole.Replicas)
	if len(roleList) == expectedCount {
		klog.V(4).Infof("The replicas of role %s in ServingGroup %s is consistent, no need to scale up or down", targetRole.Name, groupName)
		return
	}

	// Determine whether it is a scale-up or scale-down scenario
	if len(roleList) < expectedCount {
		// Handle scale up by calling scaleUpRoles
		c.scaleUpRoles(ctx, mi, groupName, targetRole, roleList, expectedCount, servingGroupOrdinal, newRevision)
	} else {
		// Handle scale down by calling scaleDownRoles
		c.scaleDownRoles(ctx, mi, groupName, targetRole, roleList, expectedCount)
	}
}

func (c *ModelServingController) DeleteRole(ctx context.Context, mi *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) {
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.GroupNameLabelKey: groupName,
		workloadv1alpha1.RoleLabelKey:      roleName,
		workloadv1alpha1.RoleIDKey:         roleID,
	})
	// If the role is already in the deletion process, no further processing will be done.
	roleStatus := c.store.GetRoleStatus(utils.GetNamespaceName(mi), groupName, roleName, roleID)
	if roleStatus == datastore.RoleDeleting {
		return
	}
	err := c.store.UpdateRoleStatus(utils.GetNamespaceName(mi), groupName, roleName, roleID, datastore.RoleDeleting)
	if err != nil {
		klog.Errorf("failed to set role %s/%s status: %v", groupName, roleID, err)
		return
	}
	// Delete all pods in role
	err = c.kubeClientSet.CoreV1().Pods(mi.Namespace).DeleteCollection(
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
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", mi.Namespace, groupName, roleName, roleID)
	services, err := c.getServicesByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		klog.Errorf("failed to get service %v", err)
		return
	}
	for _, svc := range services {
		err = c.kubeClientSet.CoreV1().Services(mi.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("service %s/%s has been deleted", mi.Namespace, svc.Name)
			}
			klog.Errorf("failed to delete service %s/%s: %v", mi.Namespace, svc.Name, err)
		}
	}

	// Ensure that the role of an individual pod can be successfully enqueue.
	if c.isRoleDeleted(mi, groupName, roleName, roleID) {
		// role has been deleted, so the storage needs to be updated and need to reconcile.
		klog.V(2).Infof("role %s of servingGroup %s has been deleted", roleID, groupName)
		c.store.DeleteRole(utils.GetNamespaceName(mi), groupName, roleName, roleID)
		c.enqueueModelServing(mi)
	}
}

func (c *ModelServingController) manageServingGroupRollingUpdate(mi *workloadv1alpha1.ModelServing, revision string) error {
	// Default values is updateMin=0, maxUnavailable=1, maxSurge=0
	updateMin, maxUnavailable, _ := utils.GetRollingUpdateConfiguration(mi)
	servingGroupList, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
	if err != nil {
		return fmt.Errorf("cannot get ServingGroupList from store, err:%v", err)
	}

	unavailableCount := 0
	for _, sg := range servingGroupList {
		if sg.Status != datastore.ServingGroupRunning {
			// ServingGroup is not running, we need to wait for the status to change to running.
			// If the group fails after rolling, it will automatically be deleted and rebuilt when detecting the pod failure.
			// If the group still pending due to reasons such as being unable to be scheduled, rolling update process will stop
			// to avoid affecting other groups that are running normally.
			unavailableCount++
		}
	}
	if unavailableCount > maxUnavailable {
		klog.V(4).Infof("Reached max unavailable ServingGroups (%d), waiting for recovery before continuing update", maxUnavailable)
		return nil
	}

	allowedToUpdate := maxUnavailable - unavailableCount
	updatedCount := 0

	// we terminate the ServingGroup with the largest ordinal that does not match the update revision.
	for i := len(servingGroupList) - 1; i >= updateMin && updatedCount < allowedToUpdate; i-- {
		if c.isServingGroupOutdated(servingGroupList[i], mi.Namespace, revision) {
			// target ServingGroup is not the latest version, needs to be updated
			klog.V(2).Infof("ServingGroup %s will be terminating for update", servingGroupList[i].Name)
			c.DeleteServingGroup(mi, servingGroupList[i].Name)
			updatedCount++
		}
	}

	if updatedCount == 0 && allowedToUpdate > 0 {
		klog.V(2).Infof("all target groups of modelServing %s have been updated", mi.Name)
	}

	return nil
}

func (c *ModelServingController) handleReadyPod(mi *workloadv1alpha1.ModelServing, servingGroupName string, newPod *corev1.Pod) error {
	// Add the running pod to the global storage and try to update the ServingGroup status
	c.store.AddRunningPodToServingGroup(types.NamespacedName{
		Namespace: mi.Namespace,
		Name:      mi.Name,
	}, servingGroupName, newPod.Name, utils.PodRevision(newPod), utils.PodRoleName(newPod), utils.PodRoleID(newPod))
	ready, err := c.checkServingGroupReady(mi, servingGroupName)
	if err != nil {
		return fmt.Errorf("failed to check ServingGroup status, err: %v", err)
	}
	if ready {
		// All pods in the ServingGroup are running, so the ServingGroup status also needs to be set to running
		err = c.store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName, datastore.ServingGroupRunning)
		if err != nil {
			return fmt.Errorf("failed to set ServingGroup %s status: %v", servingGroupName, err)
		}
		klog.V(2).Infof("Update ServingGroup %s status to Running", servingGroupName)
		c.enqueueModelServing(mi)
	} else {
		klog.V(4).Infof("ServingGroup %s still creating", servingGroupName)
	}
	return nil
}

func (c *ModelServingController) handleErrorPod(mi *workloadv1alpha1.ModelServing, servingGroupName string, errPod *corev1.Pod) error {
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
		Namespace: mi.Namespace,
		Name:      mi.Name,
	}, servingGroupName, errPod.Name)
	// If the ServingGroup status is already running, the status needs to be updated
	if groupStatus := c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName); groupStatus == datastore.ServingGroupRunning {
		err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName, datastore.ServingGroupCreating)
		if err != nil {
			return fmt.Errorf("update ServingGroup status failed, err:%v", err)
		}
		klog.V(2).Infof("update ServingGroup %s to processing when pod fails", servingGroupName)
	}
	// Wait for the grace period before processing
	go c.handlePodAfterGraceTime(mi, errPod)
	// ServingGroup status may change, needs reconcile
	c.enqueueModelServing(mi)
	return nil
}

func (c *ModelServingController) handlePodAfterGraceTime(mi *workloadv1alpha1.ModelServing, errPod *corev1.Pod) {
	if mi.Spec.Template.RestartGracePeriodSeconds != nil && *mi.Spec.Template.RestartGracePeriodSeconds > 0 {
		// Wait for the grace period before making a decision
		time.Sleep(time.Duration(*mi.Spec.Template.RestartGracePeriodSeconds) * time.Second)
		klog.V(4).Infof("%s after grace time", errPod.Name)
		defer c.graceMap.Delete(utils.GetNamespaceName(errPod))

		newPod, err := c.podsLister.Pods(mi.Namespace).Get(errPod.Name)
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
			err = c.kubeClientSet.CoreV1().Pods(mi.Namespace).Delete(context.TODO(), newPod.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("cannot delete pod %s after grace time, err: %v", newPod.Name, err)
				return
			}
			klog.V(2).Infof("%s been deleted after grace time", errPod.Name)
		}
	} else {
		// grace period is not set or the grace period is 0, the deletion will be executed immediately.
		defer c.graceMap.Delete(utils.GetNamespaceName(errPod))

		err := c.kubeClientSet.CoreV1().Pods(mi.Namespace).Delete(context.TODO(), errPod.Name, metav1.DeleteOptions{})
		if err != nil {
			klog.Errorf("cannot delete pod %s when it error, err: %v", errPod.Name, err)
			return
		}
		klog.V(2).Infof("%s been deleted without grace time", errPod.Name)
	}
}

func (c *ModelServingController) handleDeletedPod(mi *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) error {
	// pod is deleted due to failure or other reasons and needs to be rebuilt according to the RecoveryPolicy
	switch mi.Spec.RecoveryPolicy {
	case workloadv1alpha1.ServingGroupRecreate:
		// Rebuild the entire ServingGroup directly
		c.DeleteServingGroup(mi, servingGroupName)
	case workloadv1alpha1.RoleRecreate:
		// If Rolling update in RoleRecreate mode, requires re-entering the queue during the pod delete event.
		if c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName) == datastore.ServingGroupDeleting {
			c.DeleteServingGroup(mi, servingGroupName)
			return nil
		} else if c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName) == datastore.ServingGroupRunning {
			// If the ServingGroup status is running when the pod fails, we need to set it to creating
			err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName, datastore.ServingGroupCreating)
			if err != nil {
				return fmt.Errorf("failed to set ServingGroup %s status: %v", servingGroupName, err)
			}
		}
		c.DeleteRole(context.Background(), mi, servingGroupName, utils.PodRoleName(pod), utils.PodRoleID(pod))
	}
	return nil
}

func (c *ModelServingController) checkServingGroupReady(mi *workloadv1alpha1.ModelServing, servingGroupName string) (bool, error) {
	// TODO: modify ServingGroupReady logic after rolling update functionality is implemented
	runningPodsNum, err := c.store.GetRunningPodNumByServingGroup(utils.GetNamespaceName(mi), servingGroupName)
	if err != nil {
		return false, err
	}
	if runningPodsNum != utils.ExpectedPodNum(mi) {
		// the number of running pods does not reach the expected number
		return false, nil
	}
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

func (c *ModelServingController) getModelServing(pod *corev1.Pod) (*workloadv1alpha1.ModelServing, string, error) {
	modelServingName, servingGroupName, ok := utils.GetModelServingAndGroupByLabel(pod.GetLabels())
	if !ok {
		return nil, "", fmt.Errorf("cannot get modelServing name and ServingGroup name from pod %s", pod.Name)
	}
	mi, err := c.modelServingLister.ModelServings(pod.Namespace).Get(modelServingName)
	if err != nil {
		return nil, "", err
	}
	return mi, servingGroupName, nil
}

// shouldSkipPodHandling checks if a pod should be skipped based on revision mismatch
func (c *ModelServingController) shouldSkipPodHandling(mi *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) bool {
	podRevision := utils.PodRevision(pod)
	servingGroup := c.store.GetServingGroup(types.NamespacedName{
		Namespace: mi.Namespace,
		Name:      mi.Name,
	}, servingGroupName)
	if servingGroup != nil && servingGroup.Revision != podRevision {
		// If the pod revision is not equal to the ServingGroup revision, we do not need to handle it.
		klog.V(4).Infof("pod %s/%s revision %s is not equal to ServingGroup %s revision %s, skip handling",
			pod.Namespace, pod.Name, podRevision, servingGroupName, servingGroup.Revision)
		return true
	}
	return false
}

func (c *ModelServingController) isServingGroupDeleted(mi *workloadv1alpha1.ModelServing, servingGroupName string) bool {
	status := c.store.GetServingGroupStatus(utils.GetNamespaceName(mi), servingGroupName)
	if status != datastore.ServingGroupDeleting {
		// It will be determined whether all resource have been deleted only when the group status is deleting.
		return false
	}
	// check whether the ServingGroup deletion has been completed
	groupNameValue := fmt.Sprintf("%s/%s", mi.Namespace, servingGroupName)
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

func (c *ModelServingController) isRoleDeleted(mi *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) bool {
	if c.store.GetRoleStatus(utils.GetNamespaceName(mi), servingGroupName, roleName, roleID) != datastore.RoleDeleting {
		// It will be determined whether all resource have been deleted only when the role status is deleting.
		return false
	}
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", mi.Namespace, servingGroupName, roleName, roleID)
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
