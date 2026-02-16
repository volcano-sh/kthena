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
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

const timeout = 30 * time.Second

// KthenaRouterValidator handles validation of ModelRoute and ModelServer resources.
type KthenaRouterValidator struct {
	httpServer *http.Server
	kubeClient kubernetes.Interface
}

// NewKthenaRouterValidator creates a new KthenaRouterValidator.
func NewKthenaRouterValidator(kubeClient kubernetes.Interface, port int) *KthenaRouterValidator {
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	return &KthenaRouterValidator{
		httpServer: server,
		kubeClient: kubeClient,
	}
}

func (v *KthenaRouterValidator) Run(ctx context.Context, tlsCertFile, tlsPrivateKey string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/validate/modelroute", v.HandleModelRoute)
	mux.HandleFunc("/validate/modelserver", v.HandleModelServer)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			klog.Errorf("failed to write health check response: %v", err)
		}
	})
	v.httpServer.Handler = mux

	// Start server
	klog.Infof("Starting webhook server on %s", v.httpServer.Addr)
	go func() {
		if err := v.httpServer.ListenAndServeTLS(tlsCertFile, tlsPrivateKey); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("failed to listen and serve validating webhook: %v", err)
		}
	}()

	// shutdown gracefully shuts down the server
	<-ctx.Done()
	v.shutdown()
}

// HandleModelRoute handles admission requests for ModelRoute resources
func (v *KthenaRouterValidator) HandleModelRoute(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, modelRoute, err := ParseModelRouteFromRequest(r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the ModelRoute
	allowed, reason := v.validateModelRoute(modelRoute)

	// Create the admission response
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}

	if !allowed {
		admissionResponse.Result = &metav1.Status{
			Message: reason,
		}
	}

	// Create the admission review response
	admissionReview.Response = &admissionResponse

	// Send the response
	if err := SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
		return
	}
}

// HandleModelServer handles admission requests for ModelServer resources
func (v *KthenaRouterValidator) HandleModelServer(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, modelServer, err := ParseModelServerFromRequest(r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the ModelServer
	allowed, reason := v.validateModelServer(modelServer)

	// Create the admission response
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}

	if !allowed {
		admissionResponse.Result = &metav1.Status{
			Message: reason,
		}
	}

	// Create the admission review response
	admissionReview.Response = &admissionResponse

	// Send the response
	if err := SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
		return
	}
}

// validateModelRoute validates the ModelRoute resource
func (v *KthenaRouterValidator) validateModelRoute(modelRoute *networkingv1alpha1.ModelRoute) (bool, string) {
	var allErrs field.ErrorList
	specField := field.NewPath("spec")

	if modelRoute.Spec.ModelName == "" && len(modelRoute.Spec.LoraAdapters) == 0 {
		allErrs = append(allErrs, field.Required(specField, "either modelName or loraAdapters must be specified"))
	}

	for i, lora := range modelRoute.Spec.LoraAdapters {
		if lora == "" {
			allErrs = append(allErrs, field.Invalid(specField.Child("loraAdapters").Index(i), lora, "lora adapter name cannot be an empty string"))
		}
	}

	rulesField := specField.Child("rules")
	for i, rule := range modelRoute.Spec.Rules {
		if rule == nil {
			continue
		}
		ruleField := rulesField.Index(i)
		if len(rule.TargetModels) == 0 {
			allErrs = append(allErrs, field.Required(ruleField.Child("targetModels"), "each rule must have at least one target model"))
		}
	}

	if len(allErrs) > 0 {
		var messages []string
		for _, err := range allErrs {
			messages = append(messages, fmt.Sprintf("  - %s", err.Error()))
		}
		return false, fmt.Sprintf("validation failed: %s", strings.Join(messages, ""))
	}
	return true, ""
}

// validateModelServer validates the ModelServer resource
func (v *KthenaRouterValidator) validateModelServer(*networkingv1alpha1.ModelServer) (bool, string) {
	return true, ""
}

func (v *KthenaRouterValidator) shutdown() {
	klog.Info("shutting down webhook server")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := v.httpServer.Shutdown(ctx); err != nil {
		klog.Errorf("failed to shutdown server: %v", err)
	}
}
