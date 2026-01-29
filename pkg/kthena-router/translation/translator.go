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

package translation

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

// InferencePoolLookup is an interface for looking up InferencePool resources
type InferencePoolLookup interface {
	GetInferencePool(key string) *inferencev1.InferencePool
}

// GenerateTranslatedModelServerName generates a name for a translated ModelServer
func GenerateTranslatedModelServerName(namespace, name string) string {
	return fmt.Sprintf("%s-%s-%s", TranslatedPrefix, namespace, name)
}

// GenerateTranslatedModelRouteName generates a name for a translated ModelRoute
func GenerateTranslatedModelRouteName(namespace, name string) string {
	return fmt.Sprintf("%s-%s-%s", TranslatedPrefix, namespace, name)
}

// TranslateInferencePoolToModelServer translates an InferencePool to a ModelServer
func TranslateInferencePoolToModelServer(ip *inferencev1.InferencePool) *aiv1alpha1.ModelServer {
	if ip == nil {
		return nil
	}

	// Log warning if multiple ports
	if len(ip.Spec.TargetPorts) > 1 {
		klog.Warningf("InferencePool %s/%s has %d target ports, using first port only",
			ip.Namespace, ip.Name, len(ip.Spec.TargetPorts))
	}

	// Get the first target port, default to 8000 if not specified
	var port int32 = 8000
	if len(ip.Spec.TargetPorts) > 0 {
		port = int32(ip.Spec.TargetPorts[0].Number)
	}

	// Convert selector labels
	matchLabels := make(map[string]string)
	for k, v := range ip.Spec.Selector.MatchLabels {
		matchLabels[string(k)] = string(v)
	}

	ms := &aiv1alpha1.ModelServer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.kthena.volcano.sh/v1alpha1",
			Kind:       "ModelServer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      GenerateTranslatedModelServerName(ip.Namespace, ip.Name),
			Namespace: ip.Namespace,
			Annotations: map[string]string{
				AnnotationTranslatedFrom:  fmt.Sprintf("%s/%s", ip.Namespace, ip.Name),
				AnnotationOriginKind:      OriginKindInferencePool,
				AnnotationOriginName:      ip.Name,
				AnnotationOriginNamespace: ip.Namespace,
			},
		},
		Spec: aiv1alpha1.ModelServerSpec{
			// Model is nil - will be determined from request body
			Model: nil,
			// Default to vLLM inference engine
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: matchLabels,
			},
			WorkloadPort: aiv1alpha1.WorkloadPort{
				Port:     port,
				Protocol: "http",
			},
		},
	}

	return ms
}

// TranslateHTTPRouteToModelRoute translates an HTTPRoute to a ModelRoute
// poolLookup is used to resolve InferencePool backend references
func TranslateHTTPRouteToModelRoute(hr *gatewayv1.HTTPRoute, poolLookup InferencePoolLookup) *aiv1alpha1.ModelRoute {
	if hr == nil {
		return nil
	}

	// Find InferencePool backend references and collect rules
	var rules []*aiv1alpha1.Rule
	var urlRewriteConfigs []URLRewriteConfig

	for ruleIdx, rule := range hr.Spec.Rules {
		var targetModels []*aiv1alpha1.TargetModel
		var urlRewrite *URLRewriteConfig

		// Extract URL rewrite filter if present
		for _, filter := range rule.Filters {
			if filter.Type == gatewayv1.HTTPRouteFilterURLRewrite && filter.URLRewrite != nil {
				urlRewrite = extractURLRewriteConfig(filter.URLRewrite, rule.Matches)
				break
			}
		}

		// Find InferencePool backend references
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Group != nil && *backendRef.Group == "inference.networking.k8s.io" &&
				backendRef.Kind != nil && *backendRef.Kind == "InferencePool" {

				// Determine namespace
				namespace := hr.Namespace
				if backendRef.Namespace != nil {
					namespace = string(*backendRef.Namespace)
				}

				// Create target model pointing to translated ModelServer
				targetModel := &aiv1alpha1.TargetModel{
					ModelServerName: GenerateTranslatedModelServerName(namespace, string(backendRef.Name)),
				}

				// Set weight if specified
				if backendRef.Weight != nil {
					weight := uint32(*backendRef.Weight)
					targetModel.Weight = &weight
				}

				targetModels = append(targetModels, targetModel)
			}
		}

		// Skip rules without InferencePool backends
		if len(targetModels) == 0 {
			continue
		}

		// Convert matches to ModelMatch
		modelMatch := translateHTTPRouteMatches(rule.Matches)

		// Store URL rewrite config if present
		if urlRewrite != nil {
			// Add matched prefix from path matches if available
			if modelMatch != nil && modelMatch.Uri != nil && modelMatch.Uri.Prefix != nil {
				urlRewrite.MatchedPrefix = *modelMatch.Uri.Prefix
			}
			urlRewriteConfigs = append(urlRewriteConfigs, *urlRewrite)
		}

		// Create rule
		aiRule := &aiv1alpha1.Rule{
			Name:         fmt.Sprintf("rule-%d", ruleIdx),
			ModelMatch:   modelMatch,
			TargetModels: targetModels,
		}

		rules = append(rules, aiRule)
	}

	// Skip if no rules with InferencePool backends were found
	if len(rules) == 0 {
		klog.V(4).Infof("HTTPRoute %s/%s has no InferencePool backend references, skipping translation",
			hr.Namespace, hr.Name)
		return nil
	}

	// Build annotations
	annotations := map[string]string{
		AnnotationTranslatedFrom:  fmt.Sprintf("%s/%s", hr.Namespace, hr.Name),
		AnnotationOriginKind:      OriginKindHTTPRoute,
		AnnotationOriginName:      hr.Name,
		AnnotationOriginNamespace: hr.Namespace,
	}

	// Store URL rewrite config in annotation if present
	if len(urlRewriteConfigs) > 0 {
		rewriteJSON, err := json.Marshal(urlRewriteConfigs)
		if err == nil {
			annotations[AnnotationURLRewrite] = string(rewriteJSON)
		} else {
			klog.Warningf("Failed to marshal URL rewrite config for HTTPRoute %s/%s: %v",
				hr.Namespace, hr.Name, err)
		}
	}

	mr := &aiv1alpha1.ModelRoute{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.kthena.volcano.sh/v1alpha1",
			Kind:       "ModelRoute",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        GenerateTranslatedModelRouteName(hr.Namespace, hr.Name),
			Namespace:   hr.Namespace,
			Annotations: annotations,
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			// Use wildcard for path-based routing
			ModelName:  "*",
			ParentRefs: hr.Spec.ParentRefs,
			Rules:      rules,
		},
	}

	return mr
}

// translateHTTPRouteMatches converts HTTPRoute matches to ModelMatch
func translateHTTPRouteMatches(matches []gatewayv1.HTTPRouteMatch) *aiv1alpha1.ModelMatch {
	if len(matches) == 0 {
		return nil
	}

	// For simplicity, use the first match
	match := matches[0]
	modelMatch := &aiv1alpha1.ModelMatch{}
	hasContent := false

	// Convert path match
	if match.Path != nil {
		modelMatch.Uri = convertPathMatch(match.Path)
		if modelMatch.Uri != nil {
			hasContent = true
		}
	}

	// Convert header matches
	if len(match.Headers) > 0 {
		modelMatch.Headers = make(map[string]*aiv1alpha1.StringMatch)
		for _, header := range match.Headers {
			sm := convertHeaderMatch(&header)
			if sm != nil {
				modelMatch.Headers[string(header.Name)] = sm
				hasContent = true
			}
		}
	}

	if !hasContent {
		return nil
	}

	return modelMatch
}

// convertPathMatch converts HTTPPathMatch to StringMatch
func convertPathMatch(pathMatch *gatewayv1.HTTPPathMatch) *aiv1alpha1.StringMatch {
	if pathMatch == nil || pathMatch.Value == nil {
		return nil
	}

	sm := &aiv1alpha1.StringMatch{}
	pathType := gatewayv1.PathMatchPathPrefix // default
	if pathMatch.Type != nil {
		pathType = *pathMatch.Type
	}

	switch pathType {
	case gatewayv1.PathMatchExact:
		sm.Exact = pathMatch.Value
	case gatewayv1.PathMatchPathPrefix:
		sm.Prefix = pathMatch.Value
	case gatewayv1.PathMatchRegularExpression:
		sm.Regex = pathMatch.Value
	default:
		sm.Prefix = pathMatch.Value
	}

	return sm
}

// convertHeaderMatch converts HTTPHeaderMatch to StringMatch
func convertHeaderMatch(headerMatch *gatewayv1.HTTPHeaderMatch) *aiv1alpha1.StringMatch {
	if headerMatch == nil {
		return nil
	}

	sm := &aiv1alpha1.StringMatch{}
	headerType := gatewayv1.HeaderMatchExact // default
	if headerMatch.Type != nil {
		headerType = *headerMatch.Type
	}

	value := string(headerMatch.Value)
	switch headerType {
	case gatewayv1.HeaderMatchExact:
		sm.Exact = &value
	case gatewayv1.HeaderMatchRegularExpression:
		sm.Regex = &value
	default:
		sm.Exact = &value
	}

	return sm
}

// extractURLRewriteConfig extracts URL rewrite configuration from HTTPURLRewriteFilter
func extractURLRewriteConfig(urlRewrite *gatewayv1.HTTPURLRewriteFilter, matches []gatewayv1.HTTPRouteMatch) *URLRewriteConfig {
	if urlRewrite == nil {
		return nil
	}

	config := &URLRewriteConfig{}
	hasContent := false

	if urlRewrite.Hostname != nil {
		config.Hostname = string(*urlRewrite.Hostname)
		hasContent = true
	}

	if urlRewrite.Path != nil {
		config.Type = string(urlRewrite.Path.Type)
		hasContent = true

		switch urlRewrite.Path.Type {
		case gatewayv1.FullPathHTTPPathModifier:
			if urlRewrite.Path.ReplaceFullPath != nil {
				config.ReplaceFullPath = *urlRewrite.Path.ReplaceFullPath
			}
		case gatewayv1.PrefixMatchHTTPPathModifier:
			if urlRewrite.Path.ReplacePrefixMatch != nil {
				config.ReplacePrefixMatch = *urlRewrite.Path.ReplacePrefixMatch
			}
			// Extract matched prefix from the first path match
			for _, match := range matches {
				if match.Path != nil && match.Path.Value != nil {
					if match.Path.Type != nil && *match.Path.Type == gatewayv1.PathMatchPathPrefix {
						config.MatchedPrefix = *match.Path.Value
						break
					}
				}
			}
		}
	}

	if !hasContent {
		return nil
	}

	return config
}

// IsTranslatedResource checks if a resource was translated from another resource
func IsTranslatedResource(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	_, exists := annotations[AnnotationTranslatedFrom]
	return exists
}

// GetTranslationMetadata extracts translation metadata from annotations
func GetTranslationMetadata(annotations map[string]string) *TranslationMetadata {
	if annotations == nil {
		return nil
	}

	_, exists := annotations[AnnotationTranslatedFrom]
	if !exists {
		return nil
	}

	return &TranslationMetadata{
		OriginKind:      annotations[AnnotationOriginKind],
		OriginName:      annotations[AnnotationOriginName],
		OriginNamespace: annotations[AnnotationOriginNamespace],
	}
}

// GetURLRewriteConfigs extracts URL rewrite configs from ModelRoute annotations
func GetURLRewriteConfigs(annotations map[string]string) []URLRewriteConfig {
	if annotations == nil {
		return nil
	}

	rewriteJSON, exists := annotations[AnnotationURLRewrite]
	if !exists {
		return nil
	}

	var configs []URLRewriteConfig
	if err := json.Unmarshal([]byte(rewriteJSON), &configs); err != nil {
		klog.Warningf("Failed to unmarshal URL rewrite config: %v", err)
		return nil
	}

	return configs
}
