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

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/klog/v2"
)

// parseAdmissionRequest parses the HTTP request and extracts the AdmissionReview and object.
func parseAdmissionRequest[T any](r *http.Request) (*admissionv1.AdmissionReview, *T, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read request body: %v", err)
	}

	// Verify the content type is accurate
	if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
		return nil, nil, fmt.Errorf("invalid Content-Type, expect application/json, got %s", contentType)
	}

	// Parse the AdmissionReview request
	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		return nil, nil, fmt.Errorf("failed to decode body: %v", err)
	}

	if admissionReview.Request == nil {
		return nil, nil, fmt.Errorf("admission review request is nil")
	}
	if len(admissionReview.Request.Object.Raw) == 0 {
		return nil, nil, fmt.Errorf("empty object in admission request")
	}

	// Get the object from the request
	var obj T
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("failed to decode object: %v", err)
	}

	return &admissionReview, &obj, nil
}

// sendAdmissionResponse sends the AdmissionReview response back to the client.
func sendAdmissionResponse(w http.ResponseWriter, admissionReview *admissionv1.AdmissionReview) error {
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
