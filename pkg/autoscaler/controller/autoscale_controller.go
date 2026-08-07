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
	"encoding/json"
	"fmt"
	"time"

	"github.com/volcano-sh/kthena/pkg/autoscaler/autoscaler"
	corev1 "k8s.io/api/core/v1"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	workloadLister "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	"istio.io/istio/pkg/util/sets"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

type AutoscaleController struct {
	// Client for k8s. Use it to call K8S API
	kubeClient kubernetes.Interface
	// client for custom resource
	client                      clientset.Interface
	autoscalingPoliciesLister   workloadLister.AutoscalingPolicyLister
	autoscalingPoliciesInformer cache.Controller
	modelServingLister          workloadLister.ModelServingLister
	modelServingInformer        cache.Controller
	podsLister                  listerv1.PodLister
	podsInformer                cache.Controller
	scalerMap                   map[string]*autoscaler.Autoscaler
	optimizerMap                map[string]*autoscaler.Optimizer
	disaggregatedScalerMap      map[string]*autoscaler.DisaggregatedAutoscaler
	syncPeriodSeconds           int
}

func NewAutoscaleController(kubeClient kubernetes.Interface, client clientset.Interface, syncPeriodSeconds int) *AutoscaleController {
	if syncPeriodSeconds <= 0 {
		syncPeriodSeconds = util.AutoscalingSyncPeriodSeconds
	}
	informerFactory := informersv1alpha1.NewSharedInformerFactory(client, 0)
	modelInferInformer := informerFactory.Workload().V1alpha1().ModelServings()
	autoscalingPoliciesInformer := informerFactory.Workload().V1alpha1().AutoscalingPolicies()

	selector, err := labels.NewRequirement(workload.GroupNameLabelKey, selection.Exists, nil)
	if err != nil {
		klog.Errorf("can not create label selector,err:%v", err)
		return nil
	}
	kubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		kubeClient, 0, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = selector.String()
		}),
	)
	podsInformer := kubeInformerFactory.Core().V1().Pods()
	ac := &AutoscaleController{
		kubeClient:                  kubeClient,
		client:                      client,
		autoscalingPoliciesLister:   autoscalingPoliciesInformer.Lister(),
		autoscalingPoliciesInformer: autoscalingPoliciesInformer.Informer(),
		modelServingLister:          modelInferInformer.Lister(),
		modelServingInformer:        modelInferInformer.Informer(),
		podsLister:                  podsInformer.Lister(),
		podsInformer:                podsInformer.Informer(),
		scalerMap:                   make(map[string]*autoscaler.Autoscaler),
		optimizerMap:                make(map[string]*autoscaler.Optimizer),
		disaggregatedScalerMap:      make(map[string]*autoscaler.DisaggregatedAutoscaler),
		syncPeriodSeconds:           syncPeriodSeconds,
	}
	return ac
}

func (ac *AutoscaleController) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	// start informers
	go ac.autoscalingPoliciesInformer.RunWithContext(ctx)
	go ac.modelServingInformer.RunWithContext(ctx)
	go ac.podsInformer.RunWithContext(ctx)
	cache.WaitForCacheSync(ctx.Done(),
		ac.autoscalingPoliciesInformer.HasSynced,
		ac.modelServingInformer.HasSynced,
		ac.podsInformer.HasSynced,
	)

	klog.Info("start autoscale controller")
	go wait.Until(func() {
		ac.Reconcile(ctx)
	}, time.Duration(ac.syncPeriodSeconds)*time.Second, nil)

	<-ctx.Done()
	klog.Info("shut down autoscale controller")
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (ac *AutoscaleController) Reconcile(ctx context.Context) {
	klog.V(4).Info("start to reconcile")
	ctx, cancel := context.WithTimeout(ctx, util.AutoscaleCtxTimeoutSeconds*time.Second)
	defer cancel()
	policies, err := ac.autoscalingPoliciesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list autoscaling policies, err: %v", err)
		return
	}

	scalerSet := sets.New[string]()
	optimizerSet := sets.New[string]()
	disaggregatedScalerSet := sets.New[string]()

	for _, policy := range policies {
		if policy.Spec.HomogeneousTarget != nil {
			scalerSet.Insert(formatAutoscalerMapKey(policy.Namespace, policy.Name, &policy.Spec.HomogeneousTarget.Target.TargetRef))
		} else if policy.Spec.HeterogeneousTarget != nil {
			optimizerSet.Insert(formatAutoscalerMapKey(policy.Namespace, policy.Name, nil))
		} else if policy.Spec.DisaggregatedTarget != nil {
			disaggregatedScalerSet.Insert(formatAutoscalerMapKey(policy.Namespace, policy.Name, &policy.Spec.DisaggregatedTarget.TargetRef))
		} else {
			klog.Warningf("no target set, policy name: %s", policy.Name)
		}
	}

	for key := range ac.scalerMap {
		if !scalerSet.Contains(key) {
			delete(ac.scalerMap, key)
		}
	}

	for key := range ac.optimizerMap {
		if !optimizerSet.Contains(key) {
			delete(ac.optimizerMap, key)
		}
	}

	for key := range ac.disaggregatedScalerMap {
		if !disaggregatedScalerSet.Contains(key) {
			delete(ac.disaggregatedScalerMap, key)
		}
	}

	for _, policy := range policies {
		err := ac.schedule(ctx, policy)
		if err != nil {
			klog.Errorf("failed to process autoscale,err: %v", err)
			continue
		}
	}
}

func (ac *AutoscaleController) updateTargetReplicas(ctx context.Context, target *workload.Target, defaultNamespace string, replicas int32) error {
	targetRef := target.TargetRef
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = defaultNamespace
	}

	if err := checkModelServingTargetRef(targetRef); err != nil {
		return err
	}

	instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name)
	if err != nil {
		return err
	}

	if instance.Spec.Replicas != nil && *instance.Spec.Replicas == replicas {
		return nil
	}
	patchBytes := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err = ac.client.WorkloadV1alpha1().ModelServings(namespaceScope).Patch(
		ctx, targetRef.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

func (ac *AutoscaleController) getTargetReplicas(target *workload.Target, defaultNamespace string) (int32, error) {
	targetRef := target.TargetRef
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = defaultNamespace
	}

	if err := checkModelServingTargetRef(targetRef); err != nil {
		return 0, err
	}
	instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name)
	if err != nil {
		return 0, err
	}
	if instance.Spec.Replicas != nil {
		return *instance.Spec.Replicas, nil
	}
	// ModelServing.spec.replicas defaults to 1; treat unset as 1 for backward compatibility.
	return 1, nil
}

func (ac *AutoscaleController) schedule(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	klog.V(2).Infof("start to process autoscaling policy %s", klog.KObj(autoscalePolicy))
	if autoscalePolicy.Spec.HeterogeneousTarget != nil {
		if err := ac.doOptimize(ctx, autoscalePolicy); err != nil {
			klog.Errorf("failed to do optimize for autoscaling policy %s: %v", klog.KObj(autoscalePolicy), err)
			return err
		}
	} else if autoscalePolicy.Spec.HomogeneousTarget != nil {
		if err := ac.doScale(ctx, autoscalePolicy); err != nil {
			klog.Errorf("failed to do scale for autoscaling policy %s: %v", klog.KObj(autoscalePolicy), err)
			return err
		}
	} else if autoscalePolicy.Spec.DisaggregatedTarget != nil {
		if err := ac.doDisaggregatedScale(ctx, autoscalePolicy); err != nil {
			klog.Errorf("failed to do disaggregated scale for autoscaling policy %s: %v", klog.KObj(autoscalePolicy), err)
			return err
		}
	} else {
		klog.Warningf("policy %s has no target configuration", autoscalePolicy.Name)
	}

	return nil
}

func (ac *AutoscaleController) doOptimize(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	key := formatAutoscalerMapKey(autoscalePolicy.Namespace, autoscalePolicy.Name, nil)
	optimizer, ok := ac.optimizerMap[key]
	if !ok || optimizer.NeedUpdate(autoscalePolicy) {
		optimizer = autoscaler.NewOptimizer(autoscalePolicy)
		ac.optimizerMap[key] = optimizer
		klog.Infof("asp: %s changed, create new optimizer", autoscalePolicy.Name)
	}
	// Fetch current replicas
	replicasMap := make(map[string]int32, len(optimizer.Meta.Config.Params))
	for _, param := range optimizer.Meta.Config.Params {
		currentInstancesCount, err := ac.getTargetReplicas(&param.Target, autoscalePolicy.Namespace)
		if err != nil {
			klog.Errorf("failed to get current replicas, err: %v", err)
			if statusErr := ac.updateHeterogeneousPolicyStatus(ctx, autoscalePolicy, nil, err, false); statusErr != nil {
				klog.Warningf("failed to update heterogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
			}
			return err
		}
		targetKey := autoscaler.HeterogeneousTargetKey(param.Target.TargetRef, autoscalePolicy.Namespace)
		replicasMap[targetKey] = currentInstancesCount
	}

	// Get recommended replicas
	recommendedInstances, err := optimizer.Optimize(ctx, ac.podsLister, autoscalePolicy, replicasMap)
	if err != nil {
		klog.Errorf("failed to do optimize, err: %v", err)
		if statusErr := ac.updateHeterogeneousPolicyStatus(ctx, autoscalePolicy, nil, err, true); statusErr != nil {
			klog.Warningf("failed to update heterogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
		}
		return err
	}

	mode := "Stable"
	if optimizer.Status != nil && optimizer.Status.IsPanicMode() {
		mode = "Panic"
	}

	targetStatuses := make([]workload.TargetScalingStatus, 0, len(optimizer.Meta.Config.Params))
	for _, param := range optimizer.Meta.Config.Params {
		targetKey := autoscaler.HeterogeneousTargetKey(param.Target.TargetRef, autoscalePolicy.Namespace)
		instancesCount, exists := recommendedInstances[targetKey]
		if !exists {
			klog.Warningf("recommended instances not exists, target: %s", targetKey)
			continue
		}
		current := replicasMap[param.Target.TargetRef.Name]
		targetStatuses = append(targetStatuses, workload.TargetScalingStatus{
			Name:            param.Target.TargetRef.Name,
			CurrentReplicas: current,
			DesiredReplicas: instancesCount,
			Mode:            mode,
		})
	}

	// Do update replicas
	for _, param := range optimizer.Meta.Config.Params {
		instancesCount, exists := recommendedInstances[param.Target.TargetRef.Name]
		if !exists {
			continue
		}
		if err := ac.updateTargetReplicas(ctx, &param.Target, autoscalePolicy.Namespace, instancesCount); err != nil {
			klog.Errorf("failed to update target kind:%s name: %s replicas:%d, err: %v", param.Target.TargetRef.Kind, param.Target.TargetRef.Name, instancesCount, err)
			if statusErr := ac.updateHeterogeneousPolicyStatus(ctx, autoscalePolicy, targetStatuses, err, true); statusErr != nil {
				klog.Warningf("failed to update heterogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
			}
			return err
		}
	}

	if statusErr := ac.updateHeterogeneousPolicyStatus(ctx, autoscalePolicy, targetStatuses, nil, true); statusErr != nil {
		klog.Warningf("failed to update heterogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
	}

	return nil
}

func (ac *AutoscaleController) doScale(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	target := autoscalePolicy.Spec.HomogeneousTarget.Target
	key := formatAutoscalerMapKey(autoscalePolicy.Namespace, autoscalePolicy.Name, &target.TargetRef)
	scaler, ok := ac.scalerMap[key]
	if !ok || scaler.NeedUpdate(autoscalePolicy) {
		scaler = autoscaler.NewAutoscaler(autoscalePolicy)
		ac.scalerMap[key] = scaler
		klog.Infof("asp: %s changed, create new scaler", autoscalePolicy.Name)
	}
	// Fetch current replicas
	currentInstancesCount, err := ac.getTargetReplicas(&target, autoscalePolicy.Namespace)
	if err != nil {
		klog.Errorf("failed to get current replicas, err: %v", err)
		if statusErr := ac.updateHomogeneousPolicyStatus(ctx, autoscalePolicy, 0, 0, "", err, false); statusErr != nil {
			klog.Warningf("failed to update homogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
		}
		return err
	}
	// Get recommended replicas
	klog.InfoS("do homogeneous scaling for target", "targetRef", target.TargetRef, "currentInstancesCount", currentInstancesCount)
	recommendedInstances, err := scaler.Scale(ctx, ac.podsLister, autoscalePolicy, currentInstancesCount)
	if err != nil {
		klog.Errorf("failed to do homogeneous scaling for target %s, err: %v", target.TargetRef.Name, err)
		if statusErr := ac.updateHomogeneousPolicyStatus(ctx, autoscalePolicy, currentInstancesCount, 0, "", err, true); statusErr != nil {
			klog.Warningf("failed to update homogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
		}
		return err
	}
	mode := "Stable"
	if scaler.Status != nil && scaler.Status.IsPanicMode() {
		mode = "Panic"
	}
	if recommendedInstances < 0 {
		if statusErr := ac.updateHomogeneousPolicyStatus(ctx, autoscalePolicy, currentInstancesCount, currentInstancesCount, mode, nil, true); statusErr != nil {
			klog.Warningf("failed to update homogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
		}
		return nil
	}
	// Do update replicas
	if err := ac.updateTargetReplicas(ctx, &target, autoscalePolicy.Namespace, recommendedInstances); err != nil {
		klog.Errorf("failed to update target replicas %s, err: %v", target.TargetRef.Name, err)
		if statusErr := ac.updateHomogeneousPolicyStatus(ctx, autoscalePolicy, currentInstancesCount, recommendedInstances, mode, err, true); statusErr != nil {
			klog.Warningf("failed to update homogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
		}
		return err
	}
	klog.InfoS("successfully update target replicas", "targetRef", target.TargetRef, "recommendedInstances", recommendedInstances)
	if statusErr := ac.updateHomogeneousPolicyStatus(ctx, autoscalePolicy, currentInstancesCount, recommendedInstances, mode, nil, true); statusErr != nil {
		klog.Warningf("failed to update homogeneous policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, statusErr)
	}
	return nil
}

// doDisaggregatedScale runs one reconcile cycle for a DisaggregatedTarget: read
// current role replicas, compute final role replicas, patch all changed roles in
// one request, and publish AutoscalingPolicy status.
func (ac *AutoscaleController) doDisaggregatedScale(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	target := autoscalePolicy.Spec.DisaggregatedTarget
	key := formatAutoscalerMapKey(autoscalePolicy.Namespace, autoscalePolicy.Name, &target.TargetRef)
	disaggregatedScaler, ok := ac.disaggregatedScalerMap[key]
	if !ok || disaggregatedScaler.NeedUpdate(autoscalePolicy) {
		disaggregatedScaler = autoscaler.NewDisaggregatedAutoscaler(autoscalePolicy)
		if disaggregatedScaler == nil {
			return fmt.Errorf("failed to create disaggregated scaler: policy or target is nil")
		}
		ac.disaggregatedScalerMap[key] = disaggregatedScaler
		klog.Infof("asp: %s changed, create new disaggregated scaler", autoscalePolicy.Name)
	}

	modelServing, err := ac.getDisaggregatedTargetModelServing(target, autoscalePolicy.Namespace)
	if err != nil {
		if err := ac.updateDisaggregatedPolicyStatus(ctx, autoscalePolicy, nil, err, false); err != nil {
			klog.Warningf("failed to update disaggregated autoscaling policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, err)
		}
		return err
	}
	currentReplicas, err := getCurrentRoleReplicas(modelServing, target.Roles)
	if err != nil {
		if err := ac.updateDisaggregatedPolicyStatus(ctx, autoscalePolicy, nil, err, false); err != nil {
			klog.Warningf("failed to update disaggregated autoscaling policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, err)
		}
		return err
	}

	result, err := disaggregatedScaler.Scale(ctx, ac.podsLister, autoscalePolicy, currentReplicas)
	if err != nil {
		if err := ac.updateDisaggregatedPolicyStatus(ctx, autoscalePolicy, nil, err, true); err != nil {
			klog.Warningf("failed to update disaggregated autoscaling policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, err)
		}

		return err
	}
	if result == nil {
		return nil
	}
	if err := ac.updateTargetRoleReplicas(ctx, target, autoscalePolicy.Namespace, finalReplicasByRole(result.Roles)); err != nil {
		if err := ac.updateDisaggregatedPolicyStatus(ctx, autoscalePolicy, result, err, true); err != nil {
			klog.Warningf("failed to update disaggregated autoscaling policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, err)
		}
		return err
	}
	if err := ac.updateDisaggregatedPolicyStatus(ctx, autoscalePolicy, result, nil, true); err != nil {
		klog.Warningf("failed to update disaggregated autoscaling policy status %s/%s: %v", autoscalePolicy.Namespace, autoscalePolicy.Name, err)
	}
	return nil
}

// finalReplicasByRole derives the patch input from RoleScaleResult instead of
// carrying a second final-replica map in DisaggregatedScaleResult. Keeping a
// single source of truth avoids divergence between the status payload and the
// replica patch payload.
func finalReplicasByRole(roles []autoscaler.RoleScaleResult) map[string]int32 {
	replicas := make(map[string]int32, len(roles))
	for _, role := range roles {
		replicas[role.Name] = role.FinalReplicas
	}
	return replicas
}

// getDisaggregatedTargetModelServing resolves the target ModelServing with the
// policy namespace as the default namespace and rejects unsupported target kinds.
func (ac *AutoscaleController) getDisaggregatedTargetModelServing(target *workload.DisaggregatedTarget, defaultNamespace string) (*workload.ModelServing, error) {
	targetRef := target.TargetRef
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = defaultNamespace
	}
	if err := checkModelServingTargetRef(targetRef); err != nil {
		return nil, err
	}
	return ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name)
}

func checkModelServingTargetRef(targetRef corev1.ObjectReference) error {
	if targetRef.Kind != "" && targetRef.Kind != workload.ModelServingKind.Kind {
		return fmt.Errorf("target ref kind %s, name: %s not supported", targetRef.Kind, targetRef.Name)
	}
	if targetRef.APIVersion == "" {
		return nil
	}
	groupVersion, err := schema.ParseGroupVersion(targetRef.APIVersion)
	if err != nil {
		return fmt.Errorf("target ref apiVersion %s, kind %s, name: %s not supported: %w", targetRef.APIVersion, targetRef.Kind, targetRef.Name, err)
	}
	if groupVersion.Group != workload.ModelServingKind.Group {
		return fmt.Errorf("target ref group %s, kind %s, name: %s not supported", groupVersion.Group, targetRef.Kind, targetRef.Name)
	}
	return nil
}

// getCurrentRoleReplicas returns the current replica count for every role named
// in the policy. A nil role.replicas uses the API default of 1.
func getCurrentRoleReplicas(modelServing *workload.ModelServing, roleParams map[string]workload.RoleScalingParam) (map[string]int32, error) {
	if modelServing == nil {
		return nil, fmt.Errorf("modelServing is nil")
	}

	roleReplicas := make(map[string]int32, len(roleParams))
	for _, role := range modelServing.Spec.Template.Roles {
		if _, needed := roleParams[role.Name]; !needed {
			continue
		}
		replicas := int32(1)
		if role.Replicas != nil {
			replicas = *role.Replicas
		}
		roleReplicas[role.Name] = replicas
	}
	for roleName := range roleParams {
		if _, exists := roleReplicas[roleName]; !exists {
			return nil, fmt.Errorf("role %s not found in model serving %s/%s", roleName, modelServing.Namespace, modelServing.Name)
		}
	}
	return roleReplicas, nil
}

// jsonPatchOperation is the minimal JSON Patch operation used to update only
// role replica fields and avoid serializing resource/template fields.
type jsonPatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// updateTargetRoleReplicas patches all changed role replicas atomically. It uses
// JSON Patch array paths because ModelServing roles are stored as a list.
func (ac *AutoscaleController) updateTargetRoleReplicas(ctx context.Context, target *workload.DisaggregatedTarget, defaultNamespace string, desiredReplicas map[string]int32) error {
	modelServing, err := ac.getDisaggregatedTargetModelServing(target, defaultNamespace)
	if err != nil {
		return err
	}
	patches := make([]jsonPatchOperation, 0, len(desiredReplicas))
	for roleName, desired := range desiredReplicas {
		roleIndex, current := getRoleReplica(modelServing, roleName)
		if roleIndex < 0 {
			return fmt.Errorf("role %s not found in model serving %s/%s", roleName, modelServing.Namespace, modelServing.Name)
		}
		if current == desired {
			continue
		}
		// Guard the array index with a JSON Patch test operation. The role index is
		// resolved from the informer cache, so the live object could be reordered by
		// another writer before the patch reaches the API server. If that happens,
		// the test fails atomically and prevents changing replicas on the wrong role.
		// Use add instead of replace because replicas is optional and may be absent;
		// JSON Patch add works for both creating the field and updating an existing
		// object member, while replace would fail when the field is omitted.
		patches = append(patches,
			jsonPatchOperation{Op: "test", Path: fmt.Sprintf("/spec/template/roles/%d/name", roleIndex), Value: roleName},
			jsonPatchOperation{Op: "add", Path: fmt.Sprintf("/spec/template/roles/%d/replicas", roleIndex), Value: desired},
		)
	}
	if len(patches) == 0 {
		return nil
	}
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return err
	}
	_, err = ac.client.WorkloadV1alpha1().ModelServings(modelServing.Namespace).Patch(
		ctx, modelServing.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	return err
}

func getRoleReplica(modelServing *workload.ModelServing, roleName string) (int, int32) {
	for i, role := range modelServing.Spec.Template.Roles {
		if role.Name != roleName {
			continue
		}
		replicas := int32(1)
		if role.Replicas != nil {
			replicas = *role.Replicas
		}
		return i, replicas
	}
	return -1, 0
}

// updateHomogeneousPolicyStatus writes status for the last homogeneous reconcile.
func (ac *AutoscaleController) updateHomogeneousPolicyStatus(ctx context.Context, policy *workload.AutoscalingPolicy, currentReplicas int32, desiredReplicas int32, mode string, reconcileErr error, targetFound bool) error {
	readFromAPI := false
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestPolicy *workload.AutoscalingPolicy
		var getErr error
		if readFromAPI {
			latestPolicy, getErr = ac.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		} else if ac.autoscalingPoliciesLister != nil {
			latestPolicy, getErr = ac.autoscalingPoliciesLister.AutoscalingPolicies(policy.Namespace).Get(policy.Name)
		} else {
			latestPolicy, getErr = ac.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		}
		if getErr != nil {
			return getErr
		}

		policyCopy := &workload.AutoscalingPolicy{
			TypeMeta:   latestPolicy.TypeMeta,
			ObjectMeta: *latestPolicy.ObjectMeta.DeepCopy(),
			Status:     *latestPolicy.Status.DeepCopy(),
		}
		policyCopy.Status.ObservedGeneration = latestPolicy.Generation
		policyCopy.Status.HomogeneousStatus = nil
		policyCopy.Status.HeterogeneousStatus = nil
		policyCopy.Status.DisaggregatedStatus = nil

		readyCondition := metav1.Condition{
			Type:               "Ready",
			ObservedGeneration: latestPolicy.Generation,
			LastTransitionTime: metav1.Now(),
		}
		targetFoundCondition := metav1.Condition{
			Type:               "TargetFound",
			ObservedGeneration: latestPolicy.Generation,
			LastTransitionTime: metav1.Now(),
		}
		if reconcileErr != nil {
			readyCondition.Status = metav1.ConditionFalse
			readyCondition.Reason = "ReconcileFailed"
			readyCondition.Message = reconcileErr.Error()
		} else {
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Reason = "Reconciled"
			readyCondition.Message = "homogeneous autoscaling policy reconciled"
		}
		if targetFound {
			targetFoundCondition.Status = metav1.ConditionTrue
			targetFoundCondition.Reason = "TargetFound"
			targetFoundCondition.Message = "target model serving found"
		} else {
			targetFoundCondition.Status = metav1.ConditionFalse
			targetFoundCondition.Reason = "TargetInvalid"
			if reconcileErr != nil {
				targetFoundCondition.Message = reconcileErr.Error()
			} else {
				targetFoundCondition.Message = "target model serving was not found"
			}
		}
		meta.SetStatusCondition(&policyCopy.Status.Conditions, readyCondition)
		meta.SetStatusCondition(&policyCopy.Status.Conditions, targetFoundCondition)

		if targetFound && reconcileErr == nil {
			var lastScaleTime *metav1.Time
			if latestPolicy.Status.HomogeneousStatus != nil {
				lastScaleTime = latestPolicy.Status.HomogeneousStatus.LastScaleTime
			}
			if currentReplicas != desiredReplicas {
				now := metav1.Now()
				lastScaleTime = &now
			}
			policyCopy.Status.HomogeneousStatus = &workload.TargetScalingStatus{
				CurrentReplicas: currentReplicas,
				DesiredReplicas: desiredReplicas,
				Mode:            mode,
				LastScaleTime:   lastScaleTime,
			}
		}

		if equality.Semantic.DeepEqual(latestPolicy.Status, policyCopy.Status) {
			return nil
		}
		_, err := ac.client.WorkloadV1alpha1().AutoscalingPolicies(policyCopy.Namespace).UpdateStatus(ctx, policyCopy, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) {
				klog.V(4).Infof("AutoscalingPolicy %s/%s status update conflicted, retrying with API read: %v", policyCopy.Namespace, policyCopy.Name, err)
				readFromAPI = true
			}
			return err
		}
		return nil
	})
}

// updateHeterogeneousPolicyStatus writes status for the last heterogeneous reconcile.
func (ac *AutoscaleController) updateHeterogeneousPolicyStatus(ctx context.Context, policy *workload.AutoscalingPolicy, targetsStatus []workload.TargetScalingStatus, reconcileErr error, targetFound bool) error {
	readFromAPI := false
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestPolicy *workload.AutoscalingPolicy
		var getErr error
		if readFromAPI {
			latestPolicy, getErr = ac.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		} else if ac.autoscalingPoliciesLister != nil {
			latestPolicy, getErr = ac.autoscalingPoliciesLister.AutoscalingPolicies(policy.Namespace).Get(policy.Name)
		} else {
			latestPolicy, getErr = ac.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		}
		if getErr != nil {
			return getErr
		}

		policyCopy := &workload.AutoscalingPolicy{
			TypeMeta:   latestPolicy.TypeMeta,
			ObjectMeta: *latestPolicy.ObjectMeta.DeepCopy(),
			Status:     *latestPolicy.Status.DeepCopy(),
		}
		policyCopy.Status.ObservedGeneration = latestPolicy.Generation
		policyCopy.Status.HomogeneousStatus = nil
		policyCopy.Status.HeterogeneousStatus = nil
		policyCopy.Status.DisaggregatedStatus = nil

		readyCondition := metav1.Condition{
			Type:               "Ready",
			ObservedGeneration: latestPolicy.Generation,
			LastTransitionTime: metav1.Now(),
		}
		targetFoundCondition := metav1.Condition{
			Type:               "TargetFound",
			ObservedGeneration: latestPolicy.Generation,
			LastTransitionTime: metav1.Now(),
		}
		if reconcileErr != nil {
			readyCondition.Status = metav1.ConditionFalse
			readyCondition.Reason = "ReconcileFailed"
			readyCondition.Message = reconcileErr.Error()
		} else {
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Reason = "Reconciled"
			readyCondition.Message = "heterogeneous autoscaling policy reconciled"
		}
		if targetFound {
			targetFoundCondition.Status = metav1.ConditionTrue
			targetFoundCondition.Reason = "TargetFound"
			targetFoundCondition.Message = "target model servings found"
		} else {
			targetFoundCondition.Status = metav1.ConditionFalse
			targetFoundCondition.Reason = "TargetInvalid"
			if reconcileErr != nil {
				targetFoundCondition.Message = reconcileErr.Error()
			} else {
				targetFoundCondition.Message = "target model servings were not found"
			}
		}
		meta.SetStatusCondition(&policyCopy.Status.Conditions, readyCondition)
		meta.SetStatusCondition(&policyCopy.Status.Conditions, targetFoundCondition)

		if targetsStatus != nil {
			prevLastScaleTimeByName := map[string]*metav1.Time{}
			if latestPolicy.Status.HeterogeneousStatus != nil {
				for _, prev := range latestPolicy.Status.HeterogeneousStatus {
					prevLastScaleTimeByName[prev.Name] = prev.LastScaleTime
				}
			}
			now := metav1.Now()
			updatedStatuses := make([]workload.TargetScalingStatus, 0, len(targetsStatus))
			for _, ts := range targetsStatus {
				lastScaleTime := prevLastScaleTimeByName[ts.Name]
				if reconcileErr == nil && ts.CurrentReplicas != ts.DesiredReplicas {
					lastScaleTime = &now
				}
				ts.LastScaleTime = lastScaleTime
				updatedStatuses = append(updatedStatuses, ts)
			}
			policyCopy.Status.HeterogeneousStatus = updatedStatuses
		}

		if equality.Semantic.DeepEqual(latestPolicy.Status, policyCopy.Status) {
			return nil
		}
		_, err := ac.client.WorkloadV1alpha1().AutoscalingPolicies(policyCopy.Namespace).UpdateStatus(ctx, policyCopy, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) {
				klog.V(4).Infof("AutoscalingPolicy %s/%s status update conflicted, retrying with API read: %v", policyCopy.Namespace, policyCopy.Name, err)
				readFromAPI = true
			}
			return err
		}
		return nil
	})
}

// updateDisaggregatedPolicyStatus writes status for the last disaggregated
// reconcile. Ready reports the overall reconcile result, while TargetFound only
// reports whether the referenced ModelServing and configured roles were resolved.
func (ac *AutoscaleController) updateDisaggregatedPolicyStatus(ctx context.Context, policy *workload.AutoscalingPolicy, result *autoscaler.DisaggregatedScaleResult, reconcileErr error, targetFound bool) error {
	readFromAPI := false
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestPolicy *workload.AutoscalingPolicy
		var getErr error
		if readFromAPI {
			latestPolicy, getErr = ac.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		} else if ac.autoscalingPoliciesLister != nil {
			latestPolicy, getErr = ac.autoscalingPoliciesLister.AutoscalingPolicies(policy.Namespace).Get(policy.Name)
		} else {
			latestPolicy, getErr = ac.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		}
		if getErr != nil {
			return getErr
		}

		policyCopy := &workload.AutoscalingPolicy{
			TypeMeta:   latestPolicy.TypeMeta,
			ObjectMeta: *latestPolicy.ObjectMeta.DeepCopy(),
			Status:     *latestPolicy.Status.DeepCopy(),
		}
		policyCopy.Status.ObservedGeneration = latestPolicy.Generation
		policyCopy.Status.HomogeneousStatus = nil
		policyCopy.Status.HeterogeneousStatus = nil
		policyCopy.Status.DisaggregatedStatus = nil

		readyCondition := metav1.Condition{
			Type:               "Ready",
			ObservedGeneration: latestPolicy.Generation,
			LastTransitionTime: metav1.Now(),
		}
		targetFoundCondition := metav1.Condition{
			Type:               "TargetFound",
			ObservedGeneration: latestPolicy.Generation,
			LastTransitionTime: metav1.Now(),
		}
		if reconcileErr != nil {
			readyCondition.Status = metav1.ConditionFalse
			readyCondition.Reason = "ReconcileFailed"
			readyCondition.Message = reconcileErr.Error()
		} else {
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Reason = "Reconciled"
			readyCondition.Message = "disaggregated autoscaling policy reconciled"
		}
		if targetFound {
			targetFoundCondition.Status = metav1.ConditionTrue
			targetFoundCondition.Reason = "TargetFound"
			targetFoundCondition.Message = "target model serving and roles found"
		} else {
			targetFoundCondition.Status = metav1.ConditionFalse
			targetFoundCondition.Reason = "TargetInvalid"
			if reconcileErr != nil {
				targetFoundCondition.Message = reconcileErr.Error()
			} else {
				targetFoundCondition.Message = "target model serving or configured roles were not found"
			}
		}
		meta.SetStatusCondition(&policyCopy.Status.Conditions, readyCondition)
		meta.SetStatusCondition(&policyCopy.Status.Conditions, targetFoundCondition)

		if result != nil {
			roleStatuses := make([]workload.TargetScalingStatus, 0, len(result.Roles))
			prevLastScaleTimeByRole := map[string]*metav1.Time{}
			if latestPolicy.Status.DisaggregatedStatus != nil {
				for _, prev := range latestPolicy.Status.DisaggregatedStatus.Roles {
					prevLastScaleTimeByRole[prev.Name] = prev.LastScaleTime
				}
			}
			now := metav1.Now()
			for _, role := range result.Roles {
				lastScaleTime := prevLastScaleTimeByRole[role.Name]
				if reconcileErr == nil && role.CurrentReplicas != role.FinalReplicas {
					lastScaleTime = &now
				}
				roleStatuses = append(roleStatuses, workload.TargetScalingStatus{
					Name:            role.Name,
					CurrentReplicas: role.CurrentReplicas,
					DesiredReplicas: role.DesiredReplicas,
					Mode:            role.Mode,
					LastScaleTime:   lastScaleTime,
				})
			}
			policyCopy.Status.DisaggregatedStatus = &workload.DisaggregatedScalingStatus{
				Roles:         roleStatuses,
				RatioStatus:   result.RatioStatus,
				RatioAdjusted: result.RatioAdjusted,
			}
		}

		if equality.Semantic.DeepEqual(latestPolicy.Status, policyCopy.Status) {
			return nil
		}
		_, err := ac.client.WorkloadV1alpha1().AutoscalingPolicies(policyCopy.Namespace).UpdateStatus(ctx, policyCopy, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) {
				klog.V(4).Infof("AutoscalingPolicy %s/%s status update conflicted, retrying with API read: %v", policyCopy.Namespace, policyCopy.Name, err)
				readFromAPI = true
			}
			return err
		}
		return nil
	})
}

func formatAutoscalerMapKey(policyNamespace, policyName string, targetRef *corev1.ObjectReference) string {
	key := policyNamespace + "/" + policyName
	if targetRef != nil {
		targetKind := targetRef.Kind
		if targetKind == "" {
			targetKind = workload.ModelServingKind.Kind
		}
		targetNamespace := targetRef.Namespace
		if targetNamespace == "" {
			targetNamespace = policyNamespace
		}
		key += "/" + targetNamespace + "/" + targetKind + "/" + targetRef.Name
	}
	return key
}
