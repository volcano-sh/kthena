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

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	// defaultDisruptionTTL defines how long a logical disruption unit is kept in the tracker to wait for Informer sync.
	defaultDisruptionTTL   = 60 * time.Second
	trackerConfigMapPrefix = "kthena-eviction-tracker-"
	trackerEntriesKey      = "entries"
)

type disruptionUnit struct {
	namespace    string
	modelServing string
	level        workloadv1alpha1.ProtectionLevelType
	groupName    string
	role         string
	roleID       string
}

type disruptionEntry struct {
	expiresAt      time.Time
	triggerPodUID  string
	triggerPodName string
}

type disruptionEntries map[string]disruptionEntry

var evictionTrackerLocks sync.Map

// EvictionHandler handles pods/eviction admission requests with concurrency safety.
type EvictionHandler struct {
	kubeClient   kubernetes.Interface
	kthenaClient clientset.Interface
	podLister    corelisters.PodLister
	msLister     workloadlisters.ModelServingLister

	// disruptionTTL controls how long a recently allowed logical unit remains
	// in the shared ConfigMap tracker to cover informer cache lag.
	disruptionTTL time.Duration
}

func NewEvictionHandler(kubeClient kubernetes.Interface, kthenaClient clientset.Interface, podLister corelisters.PodLister, msLister workloadlisters.ModelServingLister, disruptionTTL ...time.Duration) *EvictionHandler {
	ttl := defaultDisruptionTTL
	if len(disruptionTTL) > 0 && disruptionTTL[0] > 0 {
		ttl = disruptionTTL[0]
	}
	return &EvictionHandler{
		kubeClient:    kubeClient,
		kthenaClient:  kthenaClient,
		podLister:     podLister,
		msLister:      msLister,
		disruptionTTL: ttl,
	}
}

func AllowEviction(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		klog.Errorf("Failed to decode admission review: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if admissionReview.Request == nil {
		klog.Errorf("AdmissionReview request is nil")
		http.Error(w, "request is nil", http.StatusBadRequest)
		return
	}
	admissionReview.Response = &admissionv1.AdmissionResponse{
		Allowed: true,
		UID:     admissionReview.Request.UID,
	}
	response, _ := json.Marshal(admissionReview)
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		klog.Errorf("Failed to write admission response: %v", err)
	}
}

func (h *EvictionHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		klog.Errorf("Failed to decode admission review: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ar := admissionReview.Request
	if ar == nil {
		klog.Errorf("AdmissionReview request is nil")
		http.Error(w, "request is nil", http.StatusBadRequest)
		return
	}

	if ar.Resource.Resource != "pods" || ar.SubResource != "eviction" {
		h.allow(&admissionReview, w)
		return
	}

	podNamespace := ar.Namespace
	podName := ar.Name

	pod, err := h.podLister.Pods(podNamespace).Get(podName)
	if err != nil {
		listerErr := err
		pod, err = h.kubeClient.CoreV1().Pods(podNamespace).Get(r.Context(), podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Errorf("Allowing eviction because pod %s/%s was not found in lister or API server: listerErr=%v apiErr=%v", podNamespace, podName, listerErr, err)
				h.allow(&admissionReview, w)
				return
			}
			reason := fmt.Sprintf("Eviction denied: failed to get pod %s/%s from lister or API server: listerErr=%v apiErr=%v.", podNamespace, podName, listerErr, err)
			klog.Errorf("%s", reason)
			h.deny(&admissionReview, reason, w)
			return
		}
		klog.Warningf("Pod %s/%s was not found in lister but was found by live API GET; continuing eviction protection evaluation: listerErr=%v", podNamespace, podName, listerErr)
	}

	msName := pod.Labels[workloadv1alpha1.ModelServingNameLabelKey]
	if msName == "" {
		klog.Infof("Allowing eviction for pod %s/%s because it is not owned by a ModelServing", podNamespace, podName)
		h.allow(&admissionReview, w)
		return
	}

	ms, err := h.msLister.ModelServings(podNamespace).Get(msName)
	if err != nil {
		listerErr := err
		ms, err = h.kthenaClient.WorkloadV1alpha1().ModelServings(podNamespace).Get(r.Context(), msName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Errorf("Allowing eviction for pod %s/%s because ModelServing %s/%s was not found in lister or API server: listerErr=%v apiErr=%v", podNamespace, podName, podNamespace, msName, listerErr, err)
				h.allow(&admissionReview, w)
				return
			}
			reason := fmt.Sprintf("Eviction denied: failed to get ModelServing %s/%s from lister or API server for pod %s/%s: listerErr=%v apiErr=%v.", podNamespace, msName, podNamespace, podName, listerErr, err)
			klog.Errorf("%s", reason)
			h.deny(&admissionReview, reason, w)
			return
		}
		klog.Warningf("ModelServing %s/%s for pod %s/%s was not found in lister but was found by live API GET; continuing eviction protection evaluation: listerErr=%v", podNamespace, msName, podNamespace, podName, listerErr)
	}

	if !isOwnedByCurrentModelServing(pod, ms) {
		klog.Infof("Allowing eviction for pod %s/%s because it is not owned by current ModelServing %s/%s uid=%s",
			podNamespace, podName, podNamespace, msName, ms.UID)
		h.allow(&admissionReview, w)
		return
	}

	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.EvictionStrategy == nil {
		klog.Infof("Allowing eviction for pod %s/%s ModelServing %s/%s because eviction strategy is not configured", podNamespace, podName, podNamespace, msName)
		h.allow(&admissionReview, w)
		return
	}

	allowed, reason := h.checkEvictionWithTracker(r.Context(), ms, pod)

	if allowed {
		h.allow(&admissionReview, w)
	} else {
		h.deny(&admissionReview, reason, w)
	}
}

func (h *EvictionHandler) checkEvictionWithTracker(ctx context.Context, ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod) (bool, string) {
	strategy := ms.Spec.RolloutStrategy.EvictionStrategy
	lockKey := fmt.Sprintf("%s/%s", ms.Namespace, ms.Name)
	lockValue, _ := evictionTrackerLocks.LoadOrStore(lockKey, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	allPods, err := h.podLister.Pods(ms.Namespace).List(selector)
	if err != nil {
		klog.Errorf("Failed to list pods for ModelServing %s: %v", ms.Name, err)
		return true, ""
	}
	allPods = filterPodsForCurrentModelServing(allPods, ms)
	allPods = includeTargetPod(allPods, targetPod)
	allPods = h.refreshLivePodsForIncompleteObservation(ctx, ms, targetPod, strategy, allPods)

	var allowed bool
	var reason string
	attempt := 0
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		attempt++
		cm, err := h.getOrCreateTrackerConfigMap(ctx, ms)
		if err != nil {
			return err
		}

		entries, err := decodeDisruptionEntries(cm)
		if err != nil {
			return err
		}
		entriesBeforeCleanup := len(entries)
		cleanupDisruptionEntries(entries)
		allPods = h.cleanupRecoveredDisruptionEntries(ctx, ms, entries, allPods, targetPod)
		if entriesBeforeCleanup != len(entries) {
			klog.Infof("Cleaned eviction tracker entries for ModelServing %s/%s tracker=%s entriesBefore=%d entriesAfter=%d",
				ms.Namespace, ms.Name, cm.Name, entriesBeforeCleanup, len(entries))
		}

		var unit *disruptionUnit
		if strategy.ProtectionLevel == workloadv1alpha1.ProtectionLevelRole {
			allowed, reason, unit = h.checkRoleProtection(ms, targetPod, strategy, allPods, entries)
		} else {
			allowed, reason, unit = h.checkServingGroupProtection(ms, targetPod, strategy, allPods, entries)
		}
		unitKey := disruptionUnitKey(unit)
		klog.Infof("Eviction tracker decision attempt=%d modelServing=%s/%s pod=%s/%s node=%q group=%q role=%q roleID=%q protectionLevel=%s allowed=%t reason=%q tracker=%s resourceVersion=%s trackerEntries=%d trackerKeys=%v allPods=%d disruptionUnit=%q",
			attempt, ms.Namespace, ms.Name, targetPod.Namespace, targetPod.Name, targetPod.Spec.NodeName,
			targetPod.Labels[workloadv1alpha1.GroupNameLabelKey],
			targetPod.Labels[workloadv1alpha1.RoleLabelKey],
			targetPod.Labels[workloadv1alpha1.RoleIDKey],
			strategy.ProtectionLevel, allowed, reason, cm.Name, cm.ResourceVersion,
			len(entries), disruptionEntryKeys(entries), len(allPods), unitKey)
		if !allowed || unit == nil {
			return nil
		}

		expiry := time.Now().Add(h.disruptionTTL)
		entries[unitKey] = disruptionEntry{
			expiresAt:      expiry,
			triggerPodUID:  string(targetPod.UID),
			triggerPodName: targetPod.Name,
		}
		klog.Infof("Recording eviction disruption unit modelServing=%s/%s pod=%s/%s tracker=%s resourceVersion=%s unit=%q expiresAt=%s ttl=%s",
			ms.Namespace, ms.Name, targetPod.Namespace, targetPod.Name, cm.Name, cm.ResourceVersion, unitKey, expiry.Format(time.RFC3339Nano), h.disruptionTTL)
		updated := cm.DeepCopy()
		if updated.Data == nil {
			updated.Data = map[string]string{}
		}
		encoded, err := encodeDisruptionEntries(entries)
		if err != nil {
			return err
		}
		updated.Data[trackerEntriesKey] = encoded
		updatedCM, err := h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			klog.Warningf("Failed to update eviction disruption tracker modelServing=%s/%s tracker=%s previousResourceVersion=%s unit=%q entries=%d: %v",
				ms.Namespace, ms.Name, cm.Name, cm.ResourceVersion, unitKey, len(entries), err)
			return err
		}
		klog.Infof("Updated eviction disruption tracker modelServing=%s/%s tracker=%s previousResourceVersion=%s newResourceVersion=%s entries=%d keys=%v",
			ms.Namespace, ms.Name, cm.Name, cm.ResourceVersion, updatedCM.ResourceVersion, len(entries), disruptionEntryKeys(entries))
		return nil
	})
	if err != nil {
		klog.Errorf("Denying eviction for pod %s/%s ModelServing %s/%s because tracker update failed after conflict retries: %v",
			targetPod.Namespace, targetPod.Name, ms.Namespace, ms.Name, err)
		return false, fmt.Sprintf("Eviction denied: failed to update ModelServing %s disruption tracker: %v.", ms.Name, err)
	}
	return allowed, reason
}

func (h *EvictionHandler) checkServingGroupProtection(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, allPods []*corev1.Pod, entries disruptionEntries) (bool, string, *disruptionUnit) {
	targetGroupName := targetPod.Labels[workloadv1alpha1.GroupNameLabelKey]
	if targetGroupName == "" {
		klog.Infof("Allowing ServingGroup eviction for pod %s/%s ModelServing %s/%s because target group label is empty",
			targetPod.Namespace, targetPod.Name, ms.Namespace, ms.Name)
		return true, "", nil
	}
	targetUnit := servingGroupUnit(ms, targetGroupName)
	targetGroupDisrupted := isUnitDisrupted(entries, targetUnit)

	groups := make(map[string][]*corev1.Pod)
	for _, p := range allPods {
		gn := p.Labels[workloadv1alpha1.GroupNameLabelKey]
		if gn != "" {
			groups[gn] = append(groups[gn], p)
		}
	}

	readyGroups := 0
	targetGroupReady := false
	targetGroupFound := false
	groupStates := make([]string, 0, len(groups))
	for gn, pods := range groups {
		disrupted := isUnitDisrupted(entries, servingGroupUnit(ms, gn))
		isReady := h.isServingGroupReady(ms, gn, pods, entries)
		if isReady {
			readyGroups++
		}
		if gn == targetGroupName {
			targetGroupFound = true
			targetGroupReady = isReady
		}
		groupStates = append(groupStates, fmt.Sprintf("%s(pods=%d,ready=%t,tracked=%t)", gn, len(pods), isReady, disrupted))
	}
	sort.Strings(groupStates)

	totalReplicas := int(replicasOrDefault(ms.Spec.Replicas))
	minAvailable := -1
	if totalReplicas > 0 {
		minAvailable, _ = intstr.GetScaledValueFromIntOrPercent(minAvailableOrDefault(strategy.MinAvailable), totalReplicas, true)
	}
	if totalReplicas == 0 {
		logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, true, "ModelServing replicas is zero")
		return true, "", nil
	}

	if !targetGroupFound {
		reason := fmt.Sprintf("Eviction denied: protected by ModelServing %s. Target group %s was not observed while current ready groups (%d) <= minAvailable (%d).", ms.Name, targetGroupName, readyGroups, minAvailable)
		logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, false, reason)
		return false, reason, nil
	}

	if !targetGroupReady {
		if !isPodReady(targetPod) {
			logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, true, "target pod is already not ready")
			return true, "", nil
		}
		if targetGroupDisrupted {
			logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, true, "target group is already tracked as disrupted")
			return true, "", nil
		}
		if readyGroups > minAvailable {
			logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, true, "target group is already not ready but ready groups exceed minAvailable")
			return true, "", nil
		}
		reason := fmt.Sprintf("Eviction denied: protected by ModelServing %s. Target group %s is not ready and not tracked; current ready groups (%d) <= minAvailable (%d).", ms.Name, targetGroupName, readyGroups, minAvailable)
		logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, false, reason)
		return false, reason, nil
	}

	if readyGroups > minAvailable {
		logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, true, "ready groups exceed minAvailable")
		return true, "", &targetUnit
	}

	reason := fmt.Sprintf("Eviction denied: protected by ModelServing %s. Current ready groups (%d) <= minAvailable (%d).", ms.Name, readyGroups, minAvailable)
	logServingGroupEvictionState(ms, targetPod, targetGroupName, targetGroupFound, targetGroupReady, readyGroups, len(groups), totalReplicas, minAvailable, groupStates, false, reason)
	return false, reason, nil
}

func (h *EvictionHandler) checkRoleProtection(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, allPods []*corev1.Pod, entries disruptionEntries) (bool, string, *disruptionUnit) {
	targetGroupName := targetPod.Labels[workloadv1alpha1.GroupNameLabelKey]
	targetRole := targetPod.Labels[workloadv1alpha1.RoleLabelKey]
	targetRoleID := targetPod.Labels[workloadv1alpha1.RoleIDKey]
	if targetGroupName == "" || targetRole == "" || targetRoleID == "" {
		klog.Infof("Allowing Role eviction for pod %s/%s ModelServing %s/%s because target role labels are incomplete group=%q role=%q roleID=%q",
			targetPod.Namespace, targetPod.Name, ms.Namespace, ms.Name, targetGroupName, targetRole, targetRoleID)
		return true, "", nil
	}
	targetUnit := roleUnit(ms, targetGroupName, targetRole, targetRoleID)
	targetInstanceDisrupted := isUnitDisrupted(entries, targetUnit)

	type roleInstancePods struct {
		groupName string
		roleID    string
		pods      []*corev1.Pod
	}
	roleInstances := make(map[string]roleInstancePods)
	for _, p := range allPods {
		if p.Labels[workloadv1alpha1.GroupNameLabelKey] != targetGroupName || p.Labels[workloadv1alpha1.RoleLabelKey] != targetRole {
			continue
		}
		roleID := p.Labels[workloadv1alpha1.RoleIDKey]
		if roleID != "" {
			key := roleInstanceKey(targetGroupName, roleID)
			instance := roleInstances[key]
			instance.groupName = targetGroupName
			instance.roleID = roleID
			instance.pods = append(instance.pods, p)
			roleInstances[key] = instance
		}
	}

	readyInstances := 0
	targetInstanceReady := false
	targetInstanceFound := false
	targetInstanceKey := roleInstanceKey(targetGroupName, targetRoleID)
	roleStates := make([]string, 0, len(roleInstances))
	for key, instance := range roleInstances {
		disrupted := isUnitDisrupted(entries, roleUnit(ms, instance.groupName, targetRole, instance.roleID))
		isReady := h.isRoleInstanceReady(ms, instance.groupName, targetRole, instance.roleID, instance.pods, entries)
		if isReady {
			readyInstances++
		}
		if key == targetInstanceKey {
			targetInstanceFound = true
			targetInstanceReady = isReady
		}
		roleStates = append(roleStates, fmt.Sprintf("%s/%s(pods=%d,ready=%t,tracked=%t)", instance.groupName, instance.roleID, len(instance.pods), isReady, disrupted))
	}
	sort.Strings(roleStates)

	totalInstances := expectedRoleInstancesInServingGroup(ms, targetRole, len(roleInstances))
	minAvailable := -1
	if totalInstances > 0 {
		minAvailableValue := roleMinAvailableOrDefault(strategy, targetRole)
		minAvailable, _ = intstr.GetScaledValueFromIntOrPercent(minAvailableValue, totalInstances, true)
	}
	if totalInstances == 0 {
		logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, true, "role instance count is zero")
		return true, "", nil
	}

	if !targetInstanceFound {
		reason := fmt.Sprintf("Eviction denied: protected by ModelServing %s. Target role instance %s/%s/%s was not observed while ready instances (%d) <= minAvailable (%d).", ms.Name, targetGroupName, targetRole, targetRoleID, readyInstances, minAvailable)
		logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, false, reason)
		return false, reason, nil
	}

	if !targetInstanceReady {
		if !isPodReady(targetPod) {
			logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, true, "target pod is already not ready")
			return true, "", nil
		}
		if targetInstanceDisrupted {
			logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, true, "target role instance is already tracked as disrupted")
			return true, "", nil
		}
		if readyInstances > minAvailable {
			logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, true, "target role instance is already not ready but ready instances exceed minAvailable")
			return true, "", nil
		}
		reason := fmt.Sprintf("Eviction denied: protected by ModelServing %s. Target role instance %s/%s/%s is not ready and not tracked; ready instances (%d) <= minAvailable (%d).", ms.Name, targetGroupName, targetRole, targetRoleID, readyInstances, minAvailable)
		logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, false, reason)
		return false, reason, nil
	}

	if readyInstances > minAvailable {
		logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, true, "ready role instances exceed minAvailable")
		return true, "", &targetUnit
	}

	reason := fmt.Sprintf("Eviction denied: protected by ModelServing %s. ServingGroup %s role %s ready instances (%d) <= minAvailable (%d).", ms.Name, targetGroupName, targetRole, readyInstances, minAvailable)
	logRoleEvictionState(ms, targetPod, targetRole, targetRoleID, targetInstanceFound, targetInstanceReady, readyInstances, len(roleInstances), totalInstances, minAvailable, roleStates, false, reason)
	return false, reason, nil
}

func logServingGroupEvictionState(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, targetGroupName string, targetGroupFound, targetGroupReady bool, readyGroups, observedGroups, totalReplicas, minAvailable int, groupStates []string, allowed bool, reason string) {
	klog.Infof("ServingGroup eviction state modelServing=%s/%s pod=%s/%s node=%q targetGroup=%q targetFound=%t targetReady=%t readyGroups=%d observedGroups=%d totalReplicas=%d minAvailable=%d allowed=%t reason=%q groupStates=%v",
		ms.Namespace, ms.Name, targetPod.Namespace, targetPod.Name, targetPod.Spec.NodeName, targetGroupName,
		targetGroupFound, targetGroupReady, readyGroups, observedGroups, totalReplicas, minAvailable, allowed, reason, groupStates)
}

func logRoleEvictionState(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, targetRole, targetRoleID string, targetInstanceFound, targetInstanceReady bool, readyInstances, observedInstances, totalInstances, minAvailable int, roleStates []string, allowed bool, reason string) {
	klog.Infof("Role eviction state modelServing=%s/%s pod=%s/%s node=%q targetRole=%q targetRoleID=%q targetFound=%t targetReady=%t readyInstances=%d observedInstances=%d totalInstances=%d minAvailable=%d allowed=%t reason=%q roleStates=%v",
		ms.Namespace, ms.Name, targetPod.Namespace, targetPod.Name, targetPod.Spec.NodeName, targetRole, targetRoleID,
		targetInstanceFound, targetInstanceReady, readyInstances, observedInstances, totalInstances, minAvailable, allowed, reason, roleStates)
}

func (h *EvictionHandler) isServingGroupReady(ms *workloadv1alpha1.ModelServing, groupName string, pods []*corev1.Pod, entries disruptionEntries) bool {
	if isUnitDisrupted(entries, servingGroupUnit(ms, groupName)) {
		return false
	}
	return arePodsReady(pods)
}

func (h *EvictionHandler) isRoleInstanceReady(ms *workloadv1alpha1.ModelServing, groupName, role, roleID string, pods []*corev1.Pod, entries disruptionEntries) bool {
	if isUnitDisrupted(entries, roleUnit(ms, groupName, role, roleID)) {
		return false
	}
	return arePodsReady(pods)
}

func arePodsReady(pods []*corev1.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for _, pod := range pods {
		if !isPodReady(pod) {
			return false
		}
	}
	return true
}

func includeTargetPod(pods []*corev1.Pod, targetPod *corev1.Pod) []*corev1.Pod {
	for _, pod := range pods {
		if pod.Namespace == targetPod.Namespace && pod.Name == targetPod.Name {
			return pods
		}
	}
	return append(pods, targetPod)
}

func (h *EvictionHandler) refreshLivePodsForIncompleteObservation(ctx context.Context, ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, cachedPods []*corev1.Pod) []*corev1.Pod {
	expected, observed, scope := expectedAndObservedInstances(ms, targetPod, strategy, cachedPods)
	if expected <= 0 || observed >= expected {
		return cachedPods
	}

	livePods, err := h.liveModelServingPods(ctx, ms)
	if err != nil {
		klog.Warningf("Failed to refresh live pods for incomplete eviction observation modelServing=%s/%s pod=%s/%s scope=%s observed=%d expected=%d: %v",
			ms.Namespace, ms.Name, targetPod.Namespace, targetPod.Name, scope, observed, expected, err)
		return cachedPods
	}
	livePods = includeTargetPod(livePods, targetPod)
	liveExpected, liveObserved, _ := expectedAndObservedInstances(ms, targetPod, strategy, livePods)
	klog.Infof("Refreshed live pods for incomplete eviction observation modelServing=%s/%s pod=%s/%s scope=%s cachedObserved=%d expected=%d liveObserved=%d liveExpected=%d cachedPods=%d livePods=%d",
		ms.Namespace, ms.Name, targetPod.Namespace, targetPod.Name, scope, observed, expected, liveObserved, liveExpected, len(cachedPods), len(livePods))
	return livePods
}

func expectedAndObservedInstances(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, pods []*corev1.Pod) (int, int, string) {
	if strategy.ProtectionLevel == workloadv1alpha1.ProtectionLevelRole {
		targetGroupName := targetPod.Labels[workloadv1alpha1.GroupNameLabelKey]
		targetRole := targetPod.Labels[workloadv1alpha1.RoleLabelKey]
		if targetGroupName == "" || targetRole == "" {
			return 0, 0, "role"
		}
		observed := observedRoleInstances(pods, targetGroupName, targetRole)
		expected := expectedRoleInstancesInServingGroup(ms, targetRole, observed)
		return expected, observed, fmt.Sprintf("role/%s/%s", targetGroupName, targetRole)
	}

	observed := observedServingGroups(pods)
	expected := int(replicasOrDefault(ms.Spec.Replicas))
	return expected, observed, "servingGroup"
}

func observedRoleInstances(pods []*corev1.Pod, groupName, role string) int {
	instances := map[string]struct{}{}
	for _, pod := range pods {
		if pod.Labels[workloadv1alpha1.GroupNameLabelKey] != groupName || pod.Labels[workloadv1alpha1.RoleLabelKey] != role {
			continue
		}
		roleID := pod.Labels[workloadv1alpha1.RoleIDKey]
		if roleID == "" {
			continue
		}
		instances[roleID] = struct{}{}
	}
	return len(instances)
}

func observedServingGroups(pods []*corev1.Pod) int {
	groups := map[string]struct{}{}
	for _, pod := range pods {
		groupName := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		if groupName == "" {
			continue
		}
		groups[groupName] = struct{}{}
	}
	return len(groups)
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isUnitDisrupted(entries disruptionEntries, unit disruptionUnit) bool {
	entry, ok := entries[unit.key()]
	return ok && time.Now().Before(entry.expiresAt)
}

func cleanupDisruptionEntries(entries disruptionEntries) {
	now := time.Now()
	for key, entry := range entries {
		if now.After(entry.expiresAt) {
			delete(entries, key)
		}
	}
}

func (h *EvictionHandler) cleanupRecoveredDisruptionEntries(ctx context.Context, ms *workloadv1alpha1.ModelServing, entries disruptionEntries, cachedPods []*corev1.Pod, targetPod *corev1.Pod) []*corev1.Pod {
	cleanupRecoveredDisruptionEntries(entries, cachedPods)
	if !needsLiveRecoveryCheck(entries, cachedPods) {
		return cachedPods
	}

	livePods, err := h.liveModelServingPods(ctx, ms)
	if err != nil {
		klog.Warningf("Failed to refresh live pods for recovered eviction tracker cleanup modelServing=%s/%s: %v", ms.Namespace, ms.Name, err)
		return cachedPods
	}
	livePods = includeTargetPod(livePods, targetPod)
	entriesBeforeLiveCleanup := len(entries)
	cleanupRecoveredDisruptionEntries(entries, livePods)
	if entriesBeforeLiveCleanup != len(entries) {
		klog.Infof("Cleaned recovered eviction tracker entries with live pod refresh modelServing=%s/%s entriesBefore=%d entriesAfter=%d",
			ms.Namespace, ms.Name, entriesBeforeLiveCleanup, len(entries))
		return livePods
	}
	return cachedPods
}

func cleanupRecoveredDisruptionEntries(entries disruptionEntries, pods []*corev1.Pod) {
	for key, entry := range entries {
		if entry.triggerPodUID == "" {
			continue
		}
		unit, ok := parseDisruptionUnitKey(key)
		if !ok {
			continue
		}
		unitPods := podsForDisruptionUnit(unit, pods)
		if len(unitPods) == 0 || !arePodsReady(unitPods) {
			continue
		}
		if !hasPodUID(unitPods, entry.triggerPodUID) {
			delete(entries, key)
		}
	}
}

func needsLiveRecoveryCheck(entries disruptionEntries, pods []*corev1.Pod) bool {
	for key, entry := range entries {
		if entry.triggerPodUID == "" {
			continue
		}
		unit, ok := parseDisruptionUnitKey(key)
		if !ok {
			continue
		}
		unitPods := podsForDisruptionUnit(unit, pods)
		if len(unitPods) == 0 || !arePodsReady(unitPods) || hasPodUID(unitPods, entry.triggerPodUID) {
			return true
		}
	}
	return false
}

func (h *EvictionHandler) liveModelServingPods(ctx context.Context, ms *workloadv1alpha1.ModelServing) ([]*corev1.Pod, error) {
	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	podList, err := h.kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := podList.Items[i].DeepCopy()
		if isOwnedByCurrentModelServing(pod, ms) {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

func podsForDisruptionUnit(unit disruptionUnit, pods []*corev1.Pod) []*corev1.Pod {
	unitPods := make([]*corev1.Pod, 0)
	for _, pod := range pods {
		if pod.Namespace != unit.namespace {
			continue
		}
		if pod.Labels[workloadv1alpha1.ModelServingNameLabelKey] != unit.modelServing {
			continue
		}
		switch unit.level {
		case workloadv1alpha1.ProtectionLevelRole:
			if unit.groupName != "" && pod.Labels[workloadv1alpha1.GroupNameLabelKey] != unit.groupName {
				continue
			}
			if pod.Labels[workloadv1alpha1.RoleLabelKey] == unit.role && pod.Labels[workloadv1alpha1.RoleIDKey] == unit.roleID {
				unitPods = append(unitPods, pod)
			}
		default:
			if pod.Labels[workloadv1alpha1.GroupNameLabelKey] == unit.groupName {
				unitPods = append(unitPods, pod)
			}
		}
	}
	return unitPods
}

func hasPodUID(pods []*corev1.Pod, uid string) bool {
	for _, pod := range pods {
		if string(pod.UID) == uid {
			return true
		}
	}
	return false
}

func filterPodsForCurrentModelServing(pods []*corev1.Pod, ms *workloadv1alpha1.ModelServing) []*corev1.Pod {
	if ms.UID == "" {
		return pods
	}
	filtered := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if isOwnedByCurrentModelServing(pod, ms) {
			filtered = append(filtered, pod)
		}
	}
	return filtered
}

func isOwnedByCurrentModelServing(obj metav1.Object, ms *workloadv1alpha1.ModelServing) bool {
	if ms.UID == "" {
		return true
	}
	for _, ownerRef := range obj.GetOwnerReferences() {
		if ownerRef.APIVersion == workloadv1alpha1.SchemeGroupVersion.String() &&
			ownerRef.Kind == "ModelServing" &&
			ownerRef.Name == ms.Name &&
			ownerRef.UID == ms.UID {
			return true
		}
	}
	return false
}

func modelServingOwnerReference(ms *workloadv1alpha1.ModelServing) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
		Kind:       "ModelServing",
		Name:       ms.Name,
		UID:        ms.UID,
	}
}

func (h *EvictionHandler) getOrCreateTrackerConfigMap(ctx context.Context, ms *workloadv1alpha1.ModelServing) (*corev1.ConfigMap, error) {
	name := trackerConfigMapName(ms.Name)
	cm, err := h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if !isOwnedByCurrentModelServing(cm, ms) {
			klog.Warningf("Resetting stale eviction tracker ConfigMap %s/%s because owner does not match current ModelServing uid=%s",
				cm.Namespace, cm.Name, ms.UID)
			return h.resetTrackerConfigMap(ctx, ms, cm)
		}
		return cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			},
		},
		Data: map[string]string{
			trackerEntriesKey: "{}",
		},
	}
	if ms.UID != "" {
		cm.OwnerReferences = []metav1.OwnerReference{modelServingOwnerReference(ms)}
	}
	cm, err = h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, err)
	}
	return cm, err
}

func (h *EvictionHandler) resetTrackerConfigMap(ctx context.Context, ms *workloadv1alpha1.ModelServing, cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	updated := cm.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	updated.Labels[workloadv1alpha1.ModelServingNameLabelKey] = ms.Name
	if ms.UID != "" {
		updated.OwnerReferences = []metav1.OwnerReference{modelServingOwnerReference(ms)}
	}
	updated.Data = map[string]string{
		trackerEntriesKey: "{}",
	}
	return h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
}

func disruptionEntryKeys(entries disruptionEntries) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func disruptionUnitKey(unit *disruptionUnit) string {
	if unit == nil {
		return ""
	}
	return unit.key()
}

func trackerConfigMapName(modelServingName string) string {
	return trackerConfigMapPrefix + modelServingName
}

func decodeDisruptionEntries(cm *corev1.ConfigMap) (disruptionEntries, error) {
	if cm.Data == nil || cm.Data[trackerEntriesKey] == "" {
		return disruptionEntries{}, nil
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(cm.Data[trackerEntriesKey]), &raw); err != nil {
		return nil, fmt.Errorf("decode tracker entries: %w", err)
	}
	entries := make(disruptionEntries, len(raw))
	for key, value := range raw {
		var encodedEntry struct {
			ExpiresAt      string `json:"expiresAt"`
			TriggerPodUID  string `json:"triggerPodUID,omitempty"`
			TriggerPodName string `json:"triggerPodName,omitempty"`
		}
		if err := json.Unmarshal(value, &encodedEntry); err == nil && encodedEntry.ExpiresAt != "" {
			expiry, err := time.Parse(time.RFC3339Nano, encodedEntry.ExpiresAt)
			if err != nil {
				return nil, fmt.Errorf("decode tracker entry %q expiry: %w", key, err)
			}
			entries[key] = disruptionEntry{
				expiresAt:      expiry,
				triggerPodUID:  encodedEntry.TriggerPodUID,
				triggerPodName: encodedEntry.TriggerPodName,
			}
			continue
		}

		var expiryValue string
		if err := json.Unmarshal(value, &expiryValue); err != nil {
			return nil, fmt.Errorf("decode tracker entry %q: %w", key, err)
		}
		expiry, err := time.Parse(time.RFC3339Nano, expiryValue)
		if err != nil {
			return nil, fmt.Errorf("decode tracker entry %q expiry: %w", key, err)
		}
		entries[key] = disruptionEntry{expiresAt: expiry}
	}
	return entries, nil
}

func encodeDisruptionEntries(entries disruptionEntries) (string, error) {
	raw := make(map[string]struct {
		ExpiresAt      string `json:"expiresAt"`
		TriggerPodUID  string `json:"triggerPodUID,omitempty"`
		TriggerPodName string `json:"triggerPodName,omitempty"`
	}, len(entries))
	for key, entry := range entries {
		raw[key] = struct {
			ExpiresAt      string `json:"expiresAt"`
			TriggerPodUID  string `json:"triggerPodUID,omitempty"`
			TriggerPodName string `json:"triggerPodName,omitempty"`
		}{
			ExpiresAt:      entry.expiresAt.Format(time.RFC3339Nano),
			TriggerPodUID:  entry.triggerPodUID,
			TriggerPodName: entry.triggerPodName,
		}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encode tracker entries: %w", err)
	}
	return string(data), nil
}

func servingGroupUnit(ms *workloadv1alpha1.ModelServing, groupName string) disruptionUnit {
	return disruptionUnit{
		namespace:    ms.Namespace,
		modelServing: ms.Name,
		level:        workloadv1alpha1.ProtectionLevelServingGroup,
		groupName:    groupName,
	}
}

func roleUnit(ms *workloadv1alpha1.ModelServing, groupName, role, roleID string) disruptionUnit {
	return disruptionUnit{
		namespace:    ms.Namespace,
		modelServing: ms.Name,
		level:        workloadv1alpha1.ProtectionLevelRole,
		groupName:    groupName,
		role:         role,
		roleID:       roleID,
	}
}

func (u disruptionUnit) key() string {
	switch u.level {
	case workloadv1alpha1.ProtectionLevelRole:
		return fmt.Sprintf("%s/%s/%s/%s/%s/%s", u.level, u.namespace, u.modelServing, u.groupName, u.role, u.roleID)
	default:
		return fmt.Sprintf("%s/%s/%s/%s", u.level, u.namespace, u.modelServing, u.groupName)
	}
}

func roleInstanceKey(groupName, roleID string) string {
	return groupName + "/" + roleID
}

func parseDisruptionUnitKey(key string) (disruptionUnit, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 {
		return disruptionUnit{}, false
	}
	unit := disruptionUnit{
		level:        workloadv1alpha1.ProtectionLevelType(parts[0]),
		namespace:    parts[1],
		modelServing: parts[2],
	}
	switch unit.level {
	case workloadv1alpha1.ProtectionLevelRole:
		switch len(parts) {
		case 6:
			unit.groupName = parts[3]
			unit.role = parts[4]
			unit.roleID = parts[5]
		case 5:
			// Backward compatibility with tracker entries written before groupName
			// became part of the Role disruption identity.
			unit.role = parts[3]
			unit.roleID = parts[4]
		default:
			return disruptionUnit{}, false
		}
	default:
		if len(parts) != 4 {
			return disruptionUnit{}, false
		}
		unit.groupName = parts[3]
	}
	return unit, true
}

func minAvailableOrDefault(value *intstr.IntOrString) *intstr.IntOrString {
	if value != nil {
		return value
	}
	defaultValue := intstr.FromInt(1)
	return &defaultValue
}

func roleMinAvailableOrDefault(strategy *workloadv1alpha1.EvictionStrategySpec, role string) *intstr.IntOrString {
	if strategy.RoleMinAvailable != nil {
		if value, ok := strategy.RoleMinAvailable[role]; ok {
			return &value
		}
	}
	defaultValue := intstr.FromInt(0)
	return &defaultValue
}

func expectedRoleInstancesInServingGroup(ms *workloadv1alpha1.ModelServing, roleName string, fallback int) int {
	for _, role := range ms.Spec.Template.Roles {
		if role.Name == roleName {
			return int(replicasOrDefault(role.Replicas))
		}
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

func (h *EvictionHandler) allow(review *admissionv1.AdmissionReview, w http.ResponseWriter) {
	review.Response = &admissionv1.AdmissionResponse{
		Allowed: true,
		UID:     review.Request.UID,
	}
	h.sendResponse(review, w)
}

func (h *EvictionHandler) deny(review *admissionv1.AdmissionReview, reason string, w http.ResponseWriter) {
	review.Response = &admissionv1.AdmissionResponse{
		Allowed: false,
		UID:     review.Request.UID,
		Result: &metav1.Status{
			Code:    http.StatusTooManyRequests,
			Message: reason,
		},
	}
	h.sendResponse(review, w)
}

func (h *EvictionHandler) sendResponse(review *admissionv1.AdmissionReview, w http.ResponseWriter) {
	response, _ := json.Marshal(review)
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		klog.Errorf("Failed to write admission response: %v", err)
	}
}
