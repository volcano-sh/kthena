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
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

var immutablePodIdentityLabels = []string{
	workloadv1alpha1.ModelServingNameLabelKey,
	workloadv1alpha1.GroupNameLabelKey,
	workloadv1alpha1.RoleLabelKey,
	workloadv1alpha1.RoleIDKey,
	workloadv1alpha1.EntryLabelKey,
	workloadv1alpha1.GroupInstanceIDLabelKey,
	workloadv1alpha1.RoleInstanceIDLabelKey,
}

// PodIdentityValidator rejects mutations to topology and instance-identity labels on ModelServing Pods.
type PodIdentityValidator struct{}

func NewPodIdentityValidator() *PodIdentityValidator {
	return &PodIdentityValidator{}
}

func (v *PodIdentityValidator) Handle(w http.ResponseWriter, r *http.Request) {
	admissionReview, newPod, err := utils.ParseAdmissionRequest[corev1.Pod](r)
	if err != nil {
		klog.Errorf("Failed to parse Pod admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	allowed := true
	var reason string
	if admissionReview.Request != nil && admissionReview.Request.Operation == admissionv1.Update {
		oldPod, parseErr := parseOldPodForUpdate(admissionReview)
		if parseErr != nil {
			allowed = false
			reason = parseErr.Error()
		} else if isControlledByAnyModelServing(oldPod) || isControlledByAnyModelServing(newPod) {
			allowed, reason = validatePodIdentityImmutable(oldPod, newPod)
		}
	}

	response := &admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}
	if !allowed {
		response.Result = &metav1.Status{Message: reason}
	}
	admissionReview.Response = response
	if err := utils.SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send Pod admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
	}
}

func parseOldPodForUpdate(ar *admissionv1.AdmissionReview) (*corev1.Pod, error) {
	if len(ar.Request.OldObject.Raw) == 0 {
		return nil, fmt.Errorf("missing oldObject in Pod UPDATE admission request")
	}
	var oldPod corev1.Pod
	if err := json.Unmarshal(ar.Request.OldObject.Raw, &oldPod); err != nil {
		return nil, fmt.Errorf("failed to unmarshal oldObject Pod: %w", err)
	}
	return &oldPod, nil
}

func modelServingControllerOwner(pod *corev1.Pod) *metav1.OwnerReference {
	if pod == nil {
		return nil
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil ||
		owner.APIVersion != workloadv1alpha1.SchemeGroupVersion.String() ||
		owner.Kind != workloadv1alpha1.ModelServingKind.Kind {
		return nil
	}
	return owner
}

func isControlledByAnyModelServing(pod *corev1.Pod) bool {
	return modelServingControllerOwner(pod) != nil
}

func ownerRefsEqual(a, b *metav1.OwnerReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.APIVersion == b.APIVersion &&
		a.Kind == b.Kind &&
		a.Name == b.Name &&
		a.UID == b.UID
}

func validatePodIdentityImmutable(oldPod, newPod *corev1.Pod) (bool, string) {
	if !ownerRefsEqual(modelServingControllerOwner(oldPod), modelServingControllerOwner(newPod)) {
		return false, "metadata.ownerReferences controlling ModelServing reference is immutable on ModelServing Pods"
	}
	for _, key := range immutablePodIdentityLabels {
		oldVal, oldOK := oldPod.Labels[key]
		newVal, newOK := newPod.Labels[key]
		if oldOK != newOK || oldVal != newVal {
			return false, fmt.Sprintf("metadata.labels[%q] is immutable on ModelServing Pods (was %q, got %q)", key, oldVal, newVal)
		}
	}
	return true, ""
}
