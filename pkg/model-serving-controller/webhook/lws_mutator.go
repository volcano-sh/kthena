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
	admissionReview, lws, err := utils.ParseAdmissionRequest[lwsv1.LeaderWorkerSet](r)
	if err != nil {
		klog.Errorf("Failed to parse LeaderWorkerSet admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	patch, err := createLWSPatch(admissionReview.Request.Object.Raw, lws)
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
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/rolloutStrategy/type", lwsv1.RollingUpdateStrategyType))
	}

	return json.Marshal(patch)
}
