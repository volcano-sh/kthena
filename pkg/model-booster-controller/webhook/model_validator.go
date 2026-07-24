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
	"fmt"
	"net/http"
	"path"
	"strings"

	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
)

// ModelValidator handles validation of ModelBooster resources
type ModelValidator struct {
}

// NewModelValidator creates a new ModelValidator
func NewModelValidator() *ModelValidator {
	return &ModelValidator{}
}

// Handle handles admission requests for ModelBooster resources
func (v *ModelValidator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, model, err := parseAdmissionRequest[registryv1alpha1.ModelBooster](r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the ModelBooster
	allowed, reason := v.validateModel(model)

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
	if err := sendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
		return
	}
}

// validateModel validates the ModelBooster resource
func (v *ModelValidator) validateModel(model *registryv1alpha1.ModelBooster) (bool, string) {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateBackendReplicaBounds(model)...)
	allErrs = append(allErrs, validateWorkerImages(model)...)
	allErrs = append(allErrs, validateBackendWorkerTypes(model)...)
	allErrs = append(allErrs, validatePVCURICompatibility(model)...)

	if len(allErrs) > 0 {
		// Convert field errors to a formatted multi-line error message
		var messages []string
		for _, err := range allErrs {
			messages = append(messages, fmt.Sprintf("  - %s", err.Error()))
		}
		return false, fmt.Sprintf("validation failed:\n%s", strings.Join(messages, "\n"))
	}
	return true, ""
}

func validateBackendWorkerTypes(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	backendPath := field.NewPath("spec").Child("backend")
	backend := model.Spec.Backend
	workers := backend.Workers

	if backend.Type == registryv1alpha1.ModelBackendTypeVLLM ||
		backend.Type == registryv1alpha1.ModelBackendTypeSGLang ||
		backend.Type == registryv1alpha1.ModelBackendTypeMindIE {
		if len(workers) != 1 {
			allErrs = append(allErrs, field.Invalid(
				backendPath.Child("workers"),
				len(workers),
				fmt.Sprintf("If backend type is '%s', there must be exactly one worker", backend.Type),
			))
		} else if workers[0].Type != registryv1alpha1.ModelWorkerTypeServer {
			allErrs = append(allErrs, field.Invalid(
				backendPath.Child("workers").Index(0).Child("type"),
				workers[0].Type,
				fmt.Sprintf("If backend type is '%s', the worker type must be 'server'", backend.Type),
			))
		}
	}

	if backend.Type == registryv1alpha1.ModelBackendTypeVLLMDisaggregated {
		for j, w := range workers {
			if w.Type != registryv1alpha1.ModelWorkerTypePrefill && w.Type != registryv1alpha1.ModelWorkerTypeDecode {
				allErrs = append(allErrs, field.Invalid(
					backendPath.Child("workers").Index(j).Child("type"),
					w.Type,
					"If backend type is 'vLLMDisaggregated', all workers must be type 'prefill' or 'decode'",
				))
			}
		}
	}

	// Rule 3: MindIEDisaggregated -> all workers must be 'prefill', 'decode', 'controller', or 'coordinator'
	if backend.Type == registryv1alpha1.ModelBackendTypeMindIEDisaggregated {
		validTypes := map[registryv1alpha1.ModelWorkerType]struct{}{
			registryv1alpha1.ModelWorkerTypePrefill:     {},
			registryv1alpha1.ModelWorkerTypeDecode:      {},
			registryv1alpha1.ModelWorkerTypeController:  {},
			registryv1alpha1.ModelWorkerTypeCoordinator: {},
		}
		for j, w := range workers {
			if _, ok := validTypes[w.Type]; !ok {
				allErrs = append(allErrs, field.Invalid(
					backendPath.Child("workers").Index(j).Child("type"),
					w.Type,
					"If backend type is 'MindIEDisaggregated', all workers must be type 'prefill', 'decode', 'controller', or 'coordinator' (not 'server')",
				))
			}
		}
	}
	return allErrs
}

func validateBackendReplicaBounds(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	path := field.NewPath("spec").Child("backend")
	const maxTotalReplicas = 1000000
	backend := model.Spec.Backend
	if backend.Replicas > maxTotalReplicas {
		allErrs = append(allErrs, field.Invalid(
			path.Child("replicas"),
			backend.Replicas,
			fmt.Sprintf("replicas (%d) cannot exceed %d", backend.Replicas, maxTotalReplicas),
		))
	}
	return allErrs
}

func validateWorkerImages(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	backend := model.Spec.Backend
	for j, worker := range backend.Workers {
		if worker.Image != "" {
			if err := validateImageField(worker.Image); err != nil {
				allErrs = append(allErrs, field.Invalid(
					field.NewPath("spec").Child("backend").Child("workers").Index(j).Child("image"),
					worker.Image,
					fmt.Sprintf("invalid container image reference: %v", err),
				))
			}
		}
	}
	return allErrs
}

// validatePVCURICompatibility ensures that when modelURI uses the pvc:// scheme, the
// cacheURI also uses pvc:// and that the model source path falls within the cache
// volume mount point.
//
// Background: the downloader init container mounts only the volume specified by
// cacheURI.  When modelURI is pvc://, the downloader reads a filesystem path derived
// from that URI.  If that path is not under the cacheURI mount, the file is invisible
// to the downloader and the pod fails at runtime.
func validatePVCURICompatibility(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	backend := model.Spec.Backend
	backendPath := field.NewPath("spec").Child("backend")

	if !strings.HasPrefix(backend.ModelURI, "pvc://") {
		return nil
	}

	// cacheURI must also be pvc:// so the same PVC is mounted inside the container.
	if !strings.HasPrefix(backend.CacheURI, "pvc://") {
		allErrs = append(allErrs, field.Invalid(
			backendPath.Child("cacheURI"),
			backend.CacheURI,
			"when modelURI uses pvc://, cacheURI must also use pvc://. "+
				"The downloader only has access to the volume mounted via cacheURI. "+
				"Set cacheURI to the PVC that holds the model (e.g. pvc://<claimName>) "+
				"and set modelURI to the path within that PVC "+
				"(e.g. pvc:///<claimName>/<path-to-model>)",
		))
		return allErrs
	}

	// Reject any ".." segment at any position in the raw modelURI path.
	// Checking each segment (rather than specific substrings) catches leading ".."
	// e.g. pvc://../other, which has no "/../" substring but still escapes the mount.
	rawPVCPath := strings.TrimPrefix(backend.ModelURI, "pvc://")
	for _, seg := range strings.Split(rawPVCPath, "/") {
		if seg == ".." {
			allErrs = append(allErrs, field.Invalid(
				backendPath.Child("modelURI"),
				backend.ModelURI,
				"pvc:// modelURI must not contain '..' path segments",
			))
			return allErrs
		}
	}

	// Validate that cacheURI contains exactly a PVC claim name with no slashes.
	// PVC names are Kubernetes DNS labels and cannot contain '/'.  A slashed claim
	// name (e.g. pvc://foo/bar) would pass the mount-path prefix check but make
	// buildCacheVolume set ClaimName to "foo/bar", causing pod creation to fail.
	claimName := strings.Trim(strings.TrimPrefix(backend.CacheURI, "pvc://"), "/")
	if claimName == "" || strings.Contains(claimName, "/") {
		allErrs = append(allErrs, field.Invalid(
			backendPath.Child("cacheURI"),
			backend.CacheURI,
			"pvc:// cacheURI must contain a single PVC claim name with no path separator "+
				"(e.g. pvc://<claimName>); claim names cannot contain '/'",
		))
		return allErrs
	}

	// Verify the source path is reachable through the cache volume mount point.
	// pvcModelSourcePath applies path.Clean so the normalized paths are canonical
	// before the prefix check.
	sourcePath := pvcModelSourcePath(backend.ModelURI)
	mountPath := "/" + claimName
	if sourcePath != mountPath && !strings.HasPrefix(sourcePath, mountPath+"/") {
		allErrs = append(allErrs, field.Invalid(
			backendPath.Child("modelURI"),
			backend.ModelURI,
			fmt.Sprintf(
				"PVC source path %q is not reachable via cacheURI mount %q. "+
					"The downloader only has access to PVCs mounted via cacheURI. "+
					"If the source model and the cache are on the same PVC, set cacheURI to the PVC name "+
					"and include that name as the first path segment of modelURI. "+
					"Example: cacheURI: pvc://<claimName>, modelURI: pvc:///<claimName>/<path-to-model>",
				sourcePath, mountPath,
			),
		))
	}

	return allErrs
}

// pvcModelSourcePath returns the cleaned absolute filesystem path that the PVC downloader
// will attempt to read.  It mirrors the logic of PVCDownloader._parse_pvc_path() in
// python/kthena/downloader/pvc.py: strip the pvc:// prefix, ensure a leading slash, and
// normalize with path.Clean to collapse ".." and repeated slashes so that path-traversal
// sequences cannot bypass the reachability check.
func pvcModelSourcePath(modelURI string) string {
	p := strings.TrimPrefix(modelURI, "pvc://")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// cacheVolumeMountPath returns the cleaned absolute in-container path at which the cache
// volume is mounted.  It mirrors the logic of convert.GetCachePath: take the part after
// ://, trim surrounding slashes, and prepend /.  path.Clean is applied so that the result
// is always a canonical path, matching the normalization done in pvcModelSourcePath.
func cacheVolumeMountPath(cacheURI string) string {
	const sep = "://"
	idx := strings.Index(cacheURI, sep)
	if idx < 0 {
		return ""
	}
	s := strings.Trim(cacheURI[idx+len(sep):], "/")
	if s == "" {
		return ""
	}
	return path.Clean("/" + s)
}

// validateImageField checks if a container image string is a valid Docker reference.
func validateImageField(image string) error {
	if image == "" {
		// Optional: return the error if you want to require the image field
		return nil
	}

	// Simple validation: check if image contains at least one character and no spaces
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image cannot be empty or whitespace only")
	}

	if strings.Contains(image, " ") {
		return fmt.Errorf("image cannot contain spaces")
	}

	// Basic format check: should contain at least one character
	if len(strings.TrimSpace(image)) == 0 {
		return fmt.Errorf("invalid image format")
	}

	return nil
}
