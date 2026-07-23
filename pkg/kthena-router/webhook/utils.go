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

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/klog/v2"
)

// parseAdmissionReviewFromRequest parses the HTTP request and extracts the AdmissionReview.
func parseAdmissionReviewFromRequest(r *http.Request) (*admissionv1.AdmissionReview, error) {
	// Verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		return nil, fmt.Errorf("invalid Content-Type, expected application/json, got %s", contentType)
	}

	var body []byte
	if r.Body != nil {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %v", err)
		}
		body = data
	}

	// Parse the AdmissionReview request
	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		return nil, fmt.Errorf("failed to decode body: %v", err)
	}

	return &admissionReview, nil
}

// ParseModelRouteFromRequest parses the HTTP request and extracts the AdmissionReview and ModelRoute.
func ParseModelRouteFromRequest(r *http.Request) (*admissionv1.AdmissionReview, *networkingv1alpha1.ModelRoute, error) {
	admissionReview, err := parseAdmissionReviewFromRequest(r)
	if err != nil {
		return nil, nil, err
	}

	var mr networkingv1alpha1.ModelRoute
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &mr); err != nil {
		return nil, nil, fmt.Errorf("failed to decode modelRoute: %v", err)
	}

	return admissionReview, &mr, nil
}

// ParseModelServerFromRequest parses the HTTP request and extracts the AdmissionReview and ModelServer.
func ParseModelServerFromRequest(r *http.Request) (*admissionv1.AdmissionReview, *networkingv1alpha1.ModelServer, error) {
	admissionReview, err := parseAdmissionReviewFromRequest(r)
	if err != nil {
		return nil, nil, err
	}

	var ms networkingv1alpha1.ModelServer
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &ms); err != nil {
		return nil, nil, fmt.Errorf("failed to decode modelServer: %v", err)
	}

	return admissionReview, &ms, nil
}

// ParseExternalModelProviderFromRequest parses the HTTP request and extracts the AdmissionReview and ExternalModelProvider.
func ParseExternalModelProviderFromRequest(r *http.Request) (*admissionv1.AdmissionReview, *networkingv1alpha1.ExternalModelProvider, error) {
	admissionReview, err := parseAdmissionReviewFromRequest(r)
	if err != nil {
		return nil, nil, err
	}

	var provider networkingv1alpha1.ExternalModelProvider
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &provider); err != nil {
		return nil, nil, fmt.Errorf("failed to decode externalModelProvider: %v", err)
	}

	return admissionReview, &provider, nil
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
