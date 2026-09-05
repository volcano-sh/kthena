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
	"io"
	"net/http"

	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/klog/v2"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// LWSMutator defaults the LWS fields required when Kthena replaces the native LWS webhook.
type LWSMutator struct{}

// NewLWSMutator creates an LWSMutator.
func NewLWSMutator() *LWSMutator {
	return &LWSMutator{}
}

// Handle handles admission requests for LeaderWorkerSet resources.
func (m *LWSMutator) Handle(w http.ResponseWriter, r *http.Request) {
	admissionReview, lws, raw, err := parseLWSAdmissionRequest(r)
	if err != nil {
		klog.Errorf("Failed to parse LeaderWorkerSet admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	patch, err := createLWSPatch(raw, lws)
	if err != nil {
		klog.Errorf("Failed to create LeaderWorkerSet patch: %v", err)
		http.Error(w, fmt.Sprintf("could not create patch: %v", err), http.StatusInternalServerError)
		return
	}

	patchType := admissionv1.PatchTypeJSONPatch
	admissionReview.Response = &admissionv1.AdmissionResponse{
		Allowed:   true,
		UID:       admissionReview.Request.UID,
		Patch:     patch,
		PatchType: &patchType,
	}
	if err := utils.SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send LeaderWorkerSet admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
	}
}

func parseLWSAdmissionRequest(r *http.Request) (*admissionv1.AdmissionReview, *lwsv1.LeaderWorkerSet, []byte, error) {
	if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
		return nil, nil, nil, fmt.Errorf("invalid Content-Type, expect application/json, got %s", contentType)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read request body: %v", err)
	}

	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to decode body: %v", err)
	}
	if admissionReview.Request == nil {
		return nil, nil, nil, fmt.Errorf("admission review request is nil")
	}
	raw := admissionReview.Request.Object.Raw
	if len(raw) == 0 {
		return nil, nil, nil, fmt.Errorf("empty object in admission request")
	}

	var lws lwsv1.LeaderWorkerSet
	if err := json.Unmarshal(raw, &lws); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to decode object: %v", err)
	}
	return &admissionReview, &lws, raw, nil
}

func createLWSPatch(original []byte, lws *lwsv1.LeaderWorkerSet) ([]byte, error) {
	if lws.Spec.RolloutStrategy.Type != "" {
		return []byte("[]"), nil
	}

	var object map[string]interface{}
	if err := json.Unmarshal(original, &object); err != nil {
		return nil, fmt.Errorf("failed to inspect LeaderWorkerSet: %v", err)
	}
	spec, ok := object["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("LeaderWorkerSet spec is missing or invalid")
	}

	var patch []jsonpatch.Operation
	rolloutValue, rolloutExists := spec["rolloutStrategy"]
	if !rolloutExists || rolloutValue == nil {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/rolloutStrategy", lwsv1.RolloutStrategy{
			Type: lwsv1.RollingUpdateStrategyType,
		}))
	} else {
		rollout, ok := rolloutValue.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("LeaderWorkerSet rolloutStrategy is invalid")
		}
		operation := "replace"
		if _, typeExists := rollout["type"]; !typeExists {
			operation = "add"
		}
		patch = append(patch, jsonpatch.NewOperation(operation, "/spec/rolloutStrategy/type", lwsv1.RollingUpdateStrategyType))
	}

	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal LeaderWorkerSet patch: %v", err)
	}
	return patchJSON, nil
}
