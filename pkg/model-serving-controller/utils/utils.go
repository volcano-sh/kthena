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

package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	Entry = "true"

	// condition status of ModelServingStatus
	AllGroupsIsReady         = "All Serving groups are ready"
	SomeGroupsAreProgressing = "Some groups is progressing"
	SomeGroupsAreUpdated     = "Updated Groups are"
)

func GetNamespaceName(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// ServingGroupRegex is a regular expression that extracts the parent modelServing and ordinal from the Name of an ServingGroup
var servingGroupRegex = regexp.MustCompile("(.*)-([0-9]+)$")

// GetParentNameAndOrdinal gets the name of ServingGroup's parent modelServing and ServingGroup's ordinal as extracted from its Name.
// For example, the Servinggroup name is vllm-sample-0, this function can be used to obtain the modelServing name corresponding to the ServingGroup,
// which is vllm-sample and the serial number is 0.
func GetParentNameAndOrdinal(groupName string) (string, int) {
	parent := ""
	ordinal := -1
	subMatches := servingGroupRegex.FindStringSubmatch(groupName)
	if len(subMatches) < 3 {
		return parent, ordinal
	}
	parent = subMatches[1]
	if i, err := strconv.ParseInt(subMatches[2], 10, 32); err == nil {
		ordinal = int(i)
	}
	return parent, ordinal
}

func GenerateServingGroupName(miName string, idx int) string {
	return miName + "-" + strconv.Itoa(idx)
}

func GenerateRoleID(roleName string, idx int) string {
	return roleName + "-" + strconv.Itoa(idx)
}

func GenerateControllerRevisionName(msName, revision string) string {
	return msName + "-" + revision
}

func GeneratePodName(groupName, roleName string, podIndex int) string {
	// worker-pod number starts from 1
	// For example, WorkerPodName is vllm-sample-0-prefill-1-1, represents the first worker-pod in the second replica of the prefill role
	return groupName + "-" + roleName + "-" + strconv.Itoa(podIndex)
}

func GenerateEntryPod(role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, groupName string, roleIndex int, revision, roleTemplateHash string) *corev1.Pod {
	entryPodName := GeneratePodName(groupName, GenerateRoleID(role.Name, roleIndex), 0)
	entryPod := createBasePod(role, ms, entryPodName, groupName, revision, roleTemplateHash, roleIndex)
	entryPod.ObjectMeta.Labels[workloadv1alpha1.EntryLabelKey] = Entry
	addPodLabelAndAnnotation(entryPod, role.EntryTemplate.Metadata)
	entryPod.Spec = role.EntryTemplate.Spec
	entryPod.Spec.SchedulerName = ms.Spec.SchedulerName
	// Build environment variables into each container of all pod
	envVars := createCommonEnvVars(role, entryPod, 0)
	addPodEnvVars(entryPod, envVars...)
	return entryPod
}

func GenerateWorkerPod(role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, entryPod *corev1.Pod, groupName string, roleIndex, podIndex int, revision, roleTemplateHash string) *corev1.Pod {
	if role.WorkerTemplate == nil {
		klog.Errorf("WorkerTemplate is required when workerReplicas > 0 for role %s", role.Name)
		return nil
	}

	workerPodName := GeneratePodName(groupName, GenerateRoleID(role.Name, roleIndex), podIndex)
	workerPod := createBasePod(role, ms, workerPodName, groupName, revision, roleTemplateHash, roleIndex)
	addPodLabelAndAnnotation(workerPod, role.WorkerTemplate.Metadata)
	workerPod.Spec = role.WorkerTemplate.Spec
	workerPod.Spec.SchedulerName = ms.Spec.SchedulerName
	envVars := createCommonEnvVars(role, entryPod, podIndex)
	addPodEnvVars(workerPod, envVars...)
	return workerPod
}

func createBasePod(role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing, name, groupName, revision, roleTemplateHash string, roleIndex int) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             role.Name,
				workloadv1alpha1.RoleIDKey:                GenerateRoleID(role.Name, roleIndex),
				workloadv1alpha1.RevisionLabelKey:         revision,
				workloadv1alpha1.RoleTemplateHashLabelKey: roleTemplateHash,
			},
			OwnerReferences: []metav1.OwnerReference{
				newModelServingOwnerRef(ms),
			},
		},
	}
}

func addPodLabelAndAnnotation(pod *corev1.Pod, metadata *workloadv1alpha1.Metadata) {
	if metadata == nil {
		return
	}
	if metadata.Labels != nil {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		for k, v := range metadata.Labels {
			pod.Labels[k] = v
		}
	}
	if metadata.Annotations != nil {
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		for k, v := range metadata.Annotations {
			pod.Annotations[k] = v
		}
	}
}

func createCommonEnvVars(role workloadv1alpha1.Role, entryPod *corev1.Pod, workerIndex int) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:  workloadv1alpha1.GroupSizeEnv,
			Value: strconv.Itoa(int(role.WorkerReplicas) + 1),
		},
		{
			Name: workloadv1alpha1.EntryAddressEnv,
			// entryPod name as same as headless service name
			Value: entryPod.GetName() + "." + entryPod.Namespace,
		},
		{
			Name:  workloadv1alpha1.WorkerIndexEnv,
			Value: strconv.Itoa(workerIndex),
		},
	}
}

// addPodEnvVars adds new env vars to the container.
func addPodEnvVars(pod *corev1.Pod, newEnvVars ...corev1.EnvVar) {
	if pod == nil {
		return
	}

	for i := range pod.Spec.Containers {
		addEnvVars(&pod.Spec.Containers[i], newEnvVars...)
	}

	for i := range pod.Spec.InitContainers {
		addEnvVars(&pod.Spec.InitContainers[i], newEnvVars...)
	}
}

func addEnvVars(container *corev1.Container, newEnvVars ...corev1.EnvVar) {
	if container == nil {
		return
	}
	// Used to quickly find whether the newly added environment variable already exists
	newEnvMap := make(map[string]struct{})
	for _, env := range newEnvVars {
		newEnvMap[env.Name] = struct{}{}
	}

	// Collect environment variables that need to be retained
	var retainedEnvVars []corev1.EnvVar
	for _, env := range container.Env {
		if _, exists := newEnvMap[env.Name]; !exists {
			// This environment variable does not need to be updated.
			retainedEnvVars = append(retainedEnvVars, env)
		}
	}
	// Merge existing variables that are retained and newly added variables
	container.Env = append(retainedEnvVars, newEnvVars...)
}

// newModelServingOwnerRef creates an OwnerReference pointing to the given ModelServing.
func newModelServingOwnerRef(ms *workloadv1alpha1.ModelServing) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         workloadv1alpha1.ModelServingKind.GroupVersion().String(),
		Kind:               workloadv1alpha1.ModelServingKind.Kind,
		Name:               ms.Name,
		UID:                ms.UID,
		BlockOwnerDeletion: ptr.To(true),
		Controller:         ptr.To(true),
	}
}

func CreateHeadlessService(ctx context.Context, k8sClient kubernetes.Interface, ms *workloadv1alpha1.ModelServing, serviceSelector map[string]string, groupName, roleLabel string, roleIndex int) error {
	serviceName := GeneratePodName(groupName, GenerateRoleID(roleLabel, roleIndex), 0)
	headlessService := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: ms.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				newModelServingOwnerRef(ms),
			},
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleLabel,
				workloadv1alpha1.RoleIDKey:                GenerateRoleID(roleLabel, roleIndex),
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                "None", // defines service as headless
			Selector:                 serviceSelector,
			PublishNotReadyAddresses: true,
		},
	}
	// create the service in the cluster
	klog.V(4).Infof("Creating headless service %s", headlessService.Name)
	_, err := k8sClient.CoreV1().Services(ms.Namespace).Create(ctx, &headlessService, metav1.CreateOptions{})

	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create headless service failed: %v", err)
		}
	}
	return nil
}

func GetModelServingAndGroupByLabel(podLabels map[string]string) (string, string, bool) {
	modelServingName, ok := podLabels[workloadv1alpha1.ModelServingNameLabelKey]
	if !ok {
		return "", "", false
	}
	servingGroupName, ok := podLabels[workloadv1alpha1.GroupNameLabelKey]
	if !ok {
		return "", "", false
	}
	return modelServingName, servingGroupName, true
}

// IsOwnedByModelServingWithUID returns true when the object is owned by the ModelServing with the provided UID.
func IsOwnedByModelServingWithUID(obj metav1.Object, uid types.UID) bool {
	for _, ownerRef := range obj.GetOwnerReferences() {
		if ownerRef.APIVersion == workloadv1alpha1.SchemeGroupVersion.String() &&
			ownerRef.Kind == workloadv1alpha1.ModelServingKind.Kind &&
			ownerRef.UID == uid {
			return true
		}
	}
	klog.Warningf("object %s/%s is not owned by ModelServing with UID %s", obj.GetNamespace(), obj.GetName(), uid)
	return false
}

// IsPodRunningAndReady returns true if pod is in the PodRunning Phase, if it has a condition of PodReady.
func IsPodRunningAndReady(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning && isPodReady(pod)
}

// CheckPodRevision determine if the pod's revision is compliant or not.
func CheckPodRevision(pod *corev1.Pod, revision string) bool {
	podRevision, ok := pod.Labels[workloadv1alpha1.RevisionLabelKey]
	if !ok {
		return false
	}
	return podRevision == revision
}

// ObjectRevision returns the revision label of the resource.
func ObjectRevision(obj metav1.Object) string {
	return obj.GetLabels()[workloadv1alpha1.RevisionLabelKey]
}

func ObjectRoleTemplateHash(obj metav1.Object) string {
	return obj.GetLabels()[workloadv1alpha1.RoleTemplateHashLabelKey]
}

// GetRoleName returns the role name of the resource.
func GetRoleName(resource metav1.Object) string {
	return resource.GetLabels()[workloadv1alpha1.RoleLabelKey]
}

// GetRoleID returns the role id of the resource.
func GetRoleID(resource metav1.Object) string {
	return resource.GetLabels()[workloadv1alpha1.RoleIDKey]
}

func isPodReady(pod *corev1.Pod) bool {
	return isPodReadyConditionTrue(pod.Status)
}

func isPodReadyConditionTrue(status corev1.PodStatus) bool {
	condition := getPodReadyCondition(&status)
	return condition != nil && condition.Status == corev1.ConditionTrue
}

// getPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
// copied from k8s.io/kubernetes/pkg/api/v1/pod
func getPodReadyCondition(status *corev1.PodStatus) *corev1.PodCondition {
	if status == nil || status.Conditions == nil {
		return nil
	}

	for i := range status.Conditions {
		if status.Conditions[i].Type == corev1.PodReady {
			return &status.Conditions[i]
		}
	}
	return nil
}

// IsPodTerminating returns true if pod's DeletionTimestamp has been set
func IsPodTerminating(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp != nil
}

// IsPodFailed returns true if pod has a Phase of PodFailed.
func IsPodFailed(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed
}

func ExpectedPodNum(ms *workloadv1alpha1.ModelServing) int {
	num := 0
	for _, role := range ms.Spec.Template.Roles {
		// Calculate the expected number of pod replicas when the role is running normally
		// For each role, the expected number of pods is (entryPod.num + workerPod.num) * role.replicas
		num += (1 + int(role.WorkerReplicas)) * int(*role.Replicas)
	}
	return num
}

// ContainerRestarted return true when there is any container in the pod that gets restarted
func ContainerRestarted(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
		for j := range pod.Status.InitContainerStatuses {
			stat := pod.Status.InitContainerStatuses[j]
			if stat.RestartCount > 0 {
				return true
			}
		}
		for j := range pod.Status.ContainerStatuses {
			stat := pod.Status.ContainerStatuses[j]
			if stat.RestartCount > 0 {
				return true
			}
		}
	}
	return false
}

func newCondition(condType workloadv1alpha1.ModelServingConditionType, message string) metav1.Condition {
	var conditionType, reason string
	switch condType {
	case workloadv1alpha1.ModelServingAvailable:
		conditionType = string(workloadv1alpha1.ModelServingAvailable)
		reason = "AllGroupsReady"
	case workloadv1alpha1.ModelServingProgressing:
		conditionType = string(workloadv1alpha1.ModelServingProgressing)
		reason = "GroupProgressing"
	case workloadv1alpha1.ModelServingUpdateInProgress:
		conditionType = string(workloadv1alpha1.ModelServingUpdateInProgress)
		reason = "GroupsUpdating"
	}

	return metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionStatus(corev1.ConditionTrue),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

func SetCondition(ms *workloadv1alpha1.ModelServing, progressingGroups, updatedGroups, currentGroups []int) bool {
	var newCond metav1.Condition
	found := false
	shouldUpdate := false

	partition := 0
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.RollingUpdateConfiguration != nil && ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition != nil {
		p := ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition
		if p.Type == intstr.Int {
			partition = int(p.IntVal)
		} else if ms.Spec.Replicas != nil {
			replicas := int(*ms.Spec.Replicas)
			partitionValue, err := intstr.GetScaledValueFromIntOrPercent(p, replicas, true)
			if err != nil {
				klog.ErrorS(err, "Failed to get partition from RollingUpdateConfiguration; defaulting to 0",
					"modelServingNamespace", ms.Namespace,
					"modelServingName", ms.Name,
					"partition", p.String())
			} else {
				partition = partitionValue
			}
		}
	}

	// If progressingGroups is empty, all groups are running. In addition, we still need to check revision.
	// But if the group's revision doesn't meet the requirements, then the group's status will change to deleting,
	// so when all groups are running, it means that the revision meets the requirements as well.
	if len(progressingGroups) == 0 {
		newCond = newCondition(workloadv1alpha1.ModelServingAvailable, AllGroupsIsReady)
	} else {
		message := SomeGroupsAreProgressing + ": " + fmt.Sprintf("%v", progressingGroups)
		// If the number of current groups is greater than the Partition, modelServing is still updating.
		if len(currentGroups) > partition {
			message = message + ", " + SomeGroupsAreUpdated + ": " + fmt.Sprintf("%v", updatedGroups)
			newCond = newCondition(workloadv1alpha1.ModelServingUpdateInProgress, message)
		} else {
			newCond = newCondition(workloadv1alpha1.ModelServingProgressing, message)
		}
	}

	newCond.LastTransitionTime = metav1.Now()
	for i, curCondition := range ms.Status.Conditions {
		if newCond.Type == curCondition.Type {
			if newCond.Status != curCondition.Status {
				ms.Status.Conditions[i] = newCond
				shouldUpdate = true
			}
			found = true
		} else {
			// Available and progressing/updateInprogress are not allowed to be true at the same time.
			if exclusiveConditionTypes(curCondition, newCond) && curCondition.Status == metav1.ConditionTrue && newCond.Status == metav1.ConditionTrue {
				ms.Status.Conditions[i].Status = metav1.ConditionFalse
				shouldUpdate = true
			}
		}
	}

	if newCond.Status == metav1.ConditionTrue && !found {
		ms.Status.Conditions = append(ms.Status.Conditions, newCond)
		shouldUpdate = true
	}

	return shouldUpdate
}

// This function is refer to https://github.com/kubernetes-sigs/lws/blob/main/pkg/controllers/leaderworkerset_controller.go#L840
func exclusiveConditionTypes(condition1 metav1.Condition, condition2 metav1.Condition) bool {
	if (condition1.Type == string(workloadv1alpha1.ModelServingAvailable) && condition2.Type == string(workloadv1alpha1.ModelServingProgressing)) ||
		(condition1.Type == string(workloadv1alpha1.ModelServingProgressing) && condition2.Type == string(workloadv1alpha1.ModelServingAvailable)) {
		return true
	}

	if (condition1.Type == string(workloadv1alpha1.ModelServingAvailable) && condition2.Type == string(workloadv1alpha1.ModelServingUpdateInProgress)) ||
		(condition1.Type == string(workloadv1alpha1.ModelServingUpdateInProgress) && condition2.Type == string(workloadv1alpha1.ModelServingAvailable)) {
		return true
	}

	return false
}

// ExtractPodBlockingFailure inspects pods and returns the single most-relevant
// blocking failure as a (reason, message) pair.
// Priority order: scheduling failure > image pull > init container crash > runtime container crash.
// Each priority class is scanned across ALL pods before falling through to the next
// class, so a lower-priority failure in one pod can never mask a higher-priority
// failure in another pod.
// Returns empty strings when no actionable failure is detected.
func ExtractPodBlockingFailure(pods []*corev1.Pod) (reason, message string) {
	if r, m, ok := findSchedulingFailure(pods); ok {
		return r, m
	}
	if r, m, ok := findImagePullFailure(pods); ok {
		return r, m
	}
	if r, m, ok := findInitContainerFailure(pods); ok {
		return r, m
	}
	if r, m, ok := findRuntimeContainerFailure(pods); ok {
		return r, m
	}
	return "", ""
}

// findSchedulingFailure scans all pods for a scheduling failure condition.
func findSchedulingFailure(pods []*corev1.Pod) (reason, message string, found bool) {
	for _, pod := range pods {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Message != "" {
				return "PodSchedulingFailed", cond.Message, true
			}
		}
	}
	return "", "", false
}

// findImagePullFailure scans all pods' init and runtime containers for an image pull failure.
func findImagePullFailure(pods []*corev1.Pod) (reason, message string, found bool) {
	for _, pod := range pods {
		for _, cs := range pod.Status.InitContainerStatuses {
			if r, m := extractImagePullFailure(cs); r != "" {
				return r, m, true
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if r, m := extractImagePullFailure(cs); r != "" {
				return r, m, true
			}
		}
	}
	return "", "", false
}

// findInitContainerFailure scans all pods' init containers for a non-image-pull crash.
func findInitContainerFailure(pods []*corev1.Pod) (reason, message string, found bool) {
	for _, pod := range pods {
		for _, cs := range pod.Status.InitContainerStatuses {
			if r, m := extractContainerCrashFailure(cs, "DownloaderFailed"); r != "" {
				return r, m, true
			}
		}
	}
	return "", "", false
}

// findRuntimeContainerFailure scans all pods' runtime containers for a non-image-pull crash.
func findRuntimeContainerFailure(pods []*corev1.Pod) (reason, message string, found bool) {
	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if r, m := extractContainerCrashFailure(cs, "RuntimeContainerFailed"); r != "" {
				return r, m, true
			}
		}
	}
	return "", "", false
}

// extractImagePullFailure returns an ImagePullFailed reason if cs is waiting on
// an image pull error. Returns empty strings otherwise.
func extractImagePullFailure(cs corev1.ContainerStatus) (reason, message string) {
	if cs.State.Waiting == nil {
		return "", ""
	}
	switch cs.State.Waiting.Reason {
	case "ImagePullBackOff", "ErrImagePull":
		msg := cs.State.Waiting.Message
		if msg == "" {
			msg = cs.State.Waiting.Reason
		}
		return "ImagePullFailed", msg
	}
	return "", ""
}

// extractContainerCrashFailure maps a container status to a high-level failure reason,
// excluding image pull failures which are handled as their own priority class.
// defaultReason is used for non-image-pull waiting states and for terminated containers
// with a non-zero exit code.
func extractContainerCrashFailure(cs corev1.ContainerStatus, defaultReason string) (reason, message string) {
	if cs.State.Waiting != nil {
		switch cs.State.Waiting.Reason {
		case "ImagePullBackOff", "ErrImagePull":
			return "", ""
		}
		if cs.State.Waiting.Message != "" {
			return defaultReason, cs.State.Waiting.Message
		}
	}
	if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
		msg := cs.State.Terminated.Message
		if msg == "" {
			msg = cs.State.Terminated.Reason
		}
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", cs.State.Terminated.ExitCode)
		}
		return defaultReason, msg
	}
	return "", ""
}

// ParseAdmissionRequest parses the HTTP request and extracts the AdmissionReview and ModelServing.
func ParseModelServingFromRequest(r *http.Request) (*admissionv1.AdmissionReview, *workloadv1alpha1.ModelServing, error) {
	// Verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		return nil, nil, fmt.Errorf("invalid Content-Type, expected application/json, got %s", contentType)
	}

	var body []byte
	if r.Body != nil {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read request body: %v", err)
		}
		body = data
	}

	// Parse the AdmissionReview request
	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		return nil, nil, fmt.Errorf("failed to decode body: %v", err)
	}

	var ms workloadv1alpha1.ModelServing
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &ms); err != nil {
		return nil, nil, fmt.Errorf("failed to decode modelServing: %v", err)
	}

	return &admissionReview, &ms, nil
}

// SendAdmissionResponse sends the AdmissionReview response back to the client
func SendAdmissionResponse(w http.ResponseWriter, admissionReview *admissionv1.AdmissionReview) error {
	// Send the response
	resp, err := json.Marshal(admissionReview)
	if err != nil {
		return fmt.Errorf("failed to encode response: %v", err)
	}

	klog.V(4).Infof("Sending response: %s", string(resp))
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp); err != nil {
		return fmt.Errorf("failed to write response: %v", err)
	}

	return nil
}

// getIndexKeyFromObject extract index key from objects
func getIndexKeyFromObject(obj interface{}) (map[string]string, string, bool) {
	switch v := obj.(type) {
	case *corev1.Pod:
		return v.GetLabels(), v.GetNamespace(), true
	case *corev1.Service:
		return v.GetLabels(), v.GetNamespace(), true
	case *schedulingv1beta1.PodGroup:
		return v.GetLabels(), v.GetNamespace(), true
	default:
		return nil, "", false
	}
}

func GroupNameIndexFunc(obj interface{}) ([]string, error) {
	labels, namespace, ok := getIndexKeyFromObject(obj)
	if !ok {
		return []string{}, nil
	}

	groupName, exists := labels[workloadv1alpha1.GroupNameLabelKey]
	if !exists {
		return []string{}, nil
	}
	compositeKey := fmt.Sprintf("%s/%s", namespace, groupName)
	return []string{compositeKey}, nil
}

func RoleIDIndexFunc(obj interface{}) ([]string, error) {
	labels, namespace, ok := getIndexKeyFromObject(obj)
	if !ok {
		return []string{}, nil
	}

	groupName, groupNameExists := labels[workloadv1alpha1.GroupNameLabelKey]
	roleName, roleNameExists := labels[workloadv1alpha1.RoleLabelKey]
	roleID, roleIDExists := labels[workloadv1alpha1.RoleIDKey]

	if !groupNameExists || !roleNameExists || !roleIDExists || groupName == "" || roleName == "" || roleID == "" {
		return []string{}, nil
	}

	compositeKey := fmt.Sprintf("%s/%s/%s/%s", namespace, groupName, roleName, roleID)
	return []string{compositeKey}, nil
}

func GetMaxUnavailable(ms *workloadv1alpha1.ModelServing) (int, error) {
	maxUnavailable := intstr.FromInt(1) // Default value
	replicas := int(*ms.Spec.Replicas)
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		if ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable != nil {
			maxUnavailable = *ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable
		}
	}
	// Calculate maxUnavailable as absolute numbers
	return intstr.GetScaledValueFromIntOrPercent(&maxUnavailable, replicas, false)
}

func GetMaxUnavailableForRole(role workloadv1alpha1.Role) (int, bool, error) {
	if role.MaxUnavailable == nil {
		return 0, false, nil
	}
	replicas := 1
	if role.Replicas != nil {
		replicas = int(*role.Replicas)
	}
	maxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(role.MaxUnavailable, replicas, false)
	return maxUnavailable, true, err
}
