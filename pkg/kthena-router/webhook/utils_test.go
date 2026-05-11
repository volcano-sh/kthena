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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestParseAdmissionReviewFromRequest(t *testing.T) {
	admissionReview := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
		},
	}
	body, _ := json.Marshal(admissionReview)

	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantErr     bool
	}{
		{
			name:        "valid request",
			contentType: "application/json",
			body:        body,
			wantErr:     false,
		},
		{
			name:        "invalid content-type",
			contentType: "text/plain",
			body:        body,
			wantErr:     true,
		},
		{
			name:        "invalid body",
			contentType: "application/json",
			body:        []byte(`{invalid}`),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			got, err := parseAdmissionReviewFromRequest(req)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseAdmissionReviewFromRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got.Request.UID != admissionReview.Request.UID {
				t.Errorf("parseAdmissionReviewFromRequest() got UID = %v, want %v", got.Request.UID, admissionReview.Request.UID)
			}
		})
	}
}

func TestParseModelRouteFromRequest(t *testing.T) {
	mr := &networkingv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-mr",
		},
	}
	mrRaw, _ := json.Marshal(mr)

	admissionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: mrRaw},
		},
	}
	body, _ := json.Marshal(admissionReview)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	gotAR, gotMR, err := ParseModelRouteFromRequest(req)
	if err != nil {
		t.Fatalf("ParseModelRouteFromRequest() unexpected error: %v", err)
	}

	if gotAR == nil || gotMR == nil {
		t.Fatal("ParseModelRouteFromRequest() returned nil")
	}

	if gotMR.Name != mr.Name {
		t.Errorf("ParseModelRouteFromRequest() got name = %v, want %v", gotMR.Name, mr.Name)
	}
}

func TestParseModelServerFromRequest(t *testing.T) {
	ms := &networkingv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ms",
		},
	}
	msRaw, _ := json.Marshal(ms)

	admissionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: msRaw},
		},
	}
	body, _ := json.Marshal(admissionReview)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	gotAR, gotMS, err := ParseModelServerFromRequest(req)
	if err != nil {
		t.Fatalf("ParseModelServerFromRequest() unexpected error: %v", err)
	}

	if gotAR == nil || gotMS == nil {
		t.Fatal("ParseModelServerFromRequest() returned nil")
	}

	if gotMS.Name != ms.Name {
		t.Errorf("ParseModelServerFromRequest() got name = %v, want %v", gotMS.Name, ms.Name)
	}
}

func TestSendAdmissionResponse(t *testing.T) {
	admissionReview := &admissionv1.AdmissionReview{
		Response: &admissionv1.AdmissionResponse{
			Allowed: true,
			Result: &metav1.Status{
				Message: "Success",
			},
		},
	}

	rr := httptest.NewRecorder()
	err := SendAdmissionResponse(rr, admissionReview)
	if err != nil {
		t.Fatalf("SendAdmissionResponse() unexpected error: %v", err)
	}

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("SendAdmissionResponse() status = %v, want %v", status, http.StatusOK)
	}

	if contentType := rr.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("SendAdmissionResponse() content-type = %v, want application/json", contentType)
	}

	var gotAR admissionv1.AdmissionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &gotAR); err != nil {
		t.Fatalf("SendAdmissionResponse() failed to decode body: %v", err)
	}

	if !reflect.DeepEqual(&gotAR, admissionReview) {
		t.Errorf("SendAdmissionResponse() body = %v, want %v", gotAR, admissionReview)
	}
}
