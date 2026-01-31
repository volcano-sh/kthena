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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

func TestGenerateTranslatedModelServerName(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		ipName    string
		expected  string
	}{
		{
			name:      "simple name",
			namespace: "default",
			ipName:    "my-pool",
			expected:  "translated-default-my-pool",
		},
		{
			name:      "complex namespace",
			namespace: "my-namespace",
			ipName:    "inference-pool-1",
			expected:  "translated-my-namespace-inference-pool-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateTranslatedModelServerName(tt.namespace, tt.ipName)
			if result != tt.expected {
				t.Errorf("GenerateTranslatedModelServerName(%s, %s) = %s, expected %s",
					tt.namespace, tt.ipName, result, tt.expected)
			}
		})
	}
}

func TestGenerateTranslatedModelRouteName(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		hrName    string
		expected  string
	}{
		{
			name:      "simple name",
			namespace: "default",
			hrName:    "my-route",
			expected:  "translated-default-my-route",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateTranslatedModelRouteName(tt.namespace, tt.hrName)
			if result != tt.expected {
				t.Errorf("GenerateTranslatedModelRouteName(%s, %s) = %s, expected %s",
					tt.namespace, tt.hrName, result, tt.expected)
			}
		})
	}
}

func TestTranslateInferencePoolToModelServer(t *testing.T) {
	tests := []struct {
		name           string
		inferencePool  *inferencev1.InferencePool
		expectedNil    bool
		expectedPort   int32
		expectedLabels map[string]string
	}{
		{
			name:          "nil inference pool",
			inferencePool: nil,
			expectedNil:   true,
		},
		{
			name: "basic inference pool",
			inferencePool: &inferencev1.InferencePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
				Spec: inferencev1.InferencePoolSpec{
					Selector: inferencev1.LabelSelector{
						MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
							"app": "vllm",
						},
					},
					TargetPorts: []inferencev1.Port{
						{Number: 8080},
					},
				},
			},
			expectedNil:  false,
			expectedPort: 8080,
			expectedLabels: map[string]string{
				"app": "vllm",
			},
		},
		{
			name: "inference pool with multiple ports uses first",
			inferencePool: &inferencev1.InferencePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-port-pool",
					Namespace: "test-ns",
				},
				Spec: inferencev1.InferencePoolSpec{
					Selector: inferencev1.LabelSelector{
						MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
							"app": "llm",
						},
					},
					TargetPorts: []inferencev1.Port{
						{Number: 8000},
						{Number: 9000},
					},
				},
			},
			expectedNil:  false,
			expectedPort: 8000,
			expectedLabels: map[string]string{
				"app": "llm",
			},
		},
		{
			name: "inference pool without ports defaults to 8000",
			inferencePool: &inferencev1.InferencePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-port-pool",
					Namespace: "default",
				},
				Spec: inferencev1.InferencePoolSpec{
					Selector: inferencev1.LabelSelector{
						MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
							"app": "model",
						},
					},
					TargetPorts: []inferencev1.Port{},
				},
			},
			expectedNil:  false,
			expectedPort: 8000,
			expectedLabels: map[string]string{
				"app": "model",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TranslateInferencePoolToModelServer(tt.inferencePool)

			if tt.expectedNil {
				if result != nil {
					t.Errorf("Expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("Expected non-nil result, got nil")
			}

			// Check name
			expectedName := GenerateTranslatedModelServerName(tt.inferencePool.Namespace, tt.inferencePool.Name)
			if result.Name != expectedName {
				t.Errorf("Expected name %s, got %s", expectedName, result.Name)
			}

			// Check namespace
			if result.Namespace != tt.inferencePool.Namespace {
				t.Errorf("Expected namespace %s, got %s", tt.inferencePool.Namespace, result.Namespace)
			}

			// Check port
			if result.Spec.WorkloadPort.Port != tt.expectedPort {
				t.Errorf("Expected port %d, got %d", tt.expectedPort, result.Spec.WorkloadPort.Port)
			}

			// Check labels
			if result.Spec.WorkloadSelector == nil {
				t.Fatal("Expected WorkloadSelector to be non-nil")
			}
			for k, v := range tt.expectedLabels {
				if result.Spec.WorkloadSelector.MatchLabels[k] != v {
					t.Errorf("Expected label %s=%s, got %s=%s",
						k, v, k, result.Spec.WorkloadSelector.MatchLabels[k])
				}
			}

			// Check annotations
			if result.Annotations[AnnotationOriginKind] != OriginKindInferencePool {
				t.Errorf("Expected annotation %s=%s, got %s",
					AnnotationOriginKind, OriginKindInferencePool, result.Annotations[AnnotationOriginKind])
			}
			if result.Annotations[AnnotationOriginName] != tt.inferencePool.Name {
				t.Errorf("Expected annotation %s=%s, got %s",
					AnnotationOriginName, tt.inferencePool.Name, result.Annotations[AnnotationOriginName])
			}
		})
	}
}

// mockPoolLookup implements InferencePoolLookup for testing
type mockPoolLookup struct {
	pools map[string]*inferencev1.InferencePool
}

func (m *mockPoolLookup) GetInferencePool(key string) *inferencev1.InferencePool {
	return m.pools[key]
}

func TestTranslateHTTPRouteToModelRoute(t *testing.T) {
	pathPrefixType := gatewayv1.PathMatchPathPrefix
	exactType := gatewayv1.PathMatchExact
	gatewayKind := gatewayv1.Kind("Gateway")
	inferenceGroup := gatewayv1.Group("inference.networking.k8s.io")
	inferenceKind := gatewayv1.Kind("InferencePool")

	tests := []struct {
		name             string
		httpRoute        *gatewayv1.HTTPRoute
		poolLookup       InferencePoolLookup
		expectedNil      bool
		expectedRules    int
		expectedModelNme string
	}{
		{
			name:        "nil http route",
			httpRoute:   nil,
			poolLookup:  &mockPoolLookup{},
			expectedNil: true,
		},
		{
			name: "http route without inference pool backend",
			httpRoute: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-pool-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							// No backendRefs
						},
					},
				},
			},
			poolLookup:  &mockPoolLookup{},
			expectedNil: true,
		},
		{
			name: "http route with inference pool backend",
			httpRoute: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Kind: &gatewayKind,
								Name: "my-gateway",
							},
						},
					},
					Rules: []gatewayv1.HTTPRouteRule{
						{
							Matches: []gatewayv1.HTTPRouteMatch{
								{
									Path: &gatewayv1.HTTPPathMatch{
										Type:  &pathPrefixType,
										Value: ptrString("/v1/"),
									},
								},
							},
							BackendRefs: []gatewayv1.HTTPBackendRef{
								{
									BackendRef: gatewayv1.BackendRef{
										BackendObjectReference: gatewayv1.BackendObjectReference{
											Group: &inferenceGroup,
											Kind:  &inferenceKind,
											Name:  "test-pool",
										},
									},
								},
							},
						},
					},
				},
			},
			poolLookup:       &mockPoolLookup{},
			expectedNil:      false,
			expectedRules:    1,
			expectedModelNme: "*",
		},
		{
			name: "http route with exact path match",
			httpRoute: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exact-route",
					Namespace: "test-ns",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							Matches: []gatewayv1.HTTPRouteMatch{
								{
									Path: &gatewayv1.HTTPPathMatch{
										Type:  &exactType,
										Value: ptrString("/api/chat"),
									},
								},
							},
							BackendRefs: []gatewayv1.HTTPBackendRef{
								{
									BackendRef: gatewayv1.BackendRef{
										BackendObjectReference: gatewayv1.BackendObjectReference{
											Group: &inferenceGroup,
											Kind:  &inferenceKind,
											Name:  "chat-pool",
										},
									},
								},
							},
						},
					},
				},
			},
			poolLookup:       &mockPoolLookup{},
			expectedNil:      false,
			expectedRules:    1,
			expectedModelNme: "*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TranslateHTTPRouteToModelRoute(tt.httpRoute, tt.poolLookup)

			if tt.expectedNil {
				if result != nil {
					t.Errorf("Expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("Expected non-nil result, got nil")
			}

			// Check name
			expectedName := GenerateTranslatedModelRouteName(tt.httpRoute.Namespace, tt.httpRoute.Name)
			if result.Name != expectedName {
				t.Errorf("Expected name %s, got %s", expectedName, result.Name)
			}

			// Check model name (should be wildcard for path-based routing)
			if result.Spec.ModelName != tt.expectedModelNme {
				t.Errorf("Expected model name %s, got %s", tt.expectedModelNme, result.Spec.ModelName)
			}

			// Check rules count
			if len(result.Spec.Rules) != tt.expectedRules {
				t.Errorf("Expected %d rules, got %d", tt.expectedRules, len(result.Spec.Rules))
			}

			// Check annotations
			if result.Annotations[AnnotationOriginKind] != OriginKindHTTPRoute {
				t.Errorf("Expected annotation %s=%s, got %s",
					AnnotationOriginKind, OriginKindHTTPRoute, result.Annotations[AnnotationOriginKind])
			}
		})
	}
}

func TestConvertPathMatch(t *testing.T) {
	pathPrefixType := gatewayv1.PathMatchPathPrefix
	exactType := gatewayv1.PathMatchExact
	regexType := gatewayv1.PathMatchRegularExpression

	tests := []struct {
		name            string
		pathMatch       *gatewayv1.HTTPPathMatch
		expectedNil     bool
		expectedPrefix  string
		expectedExact   string
		expectedRegex   string
	}{
		{
			name:        "nil path match",
			pathMatch:   nil,
			expectedNil: true,
		},
		{
			name: "path match without value",
			pathMatch: &gatewayv1.HTTPPathMatch{
				Type:  &pathPrefixType,
				Value: nil,
			},
			expectedNil: true,
		},
		{
			name: "prefix path match",
			pathMatch: &gatewayv1.HTTPPathMatch{
				Type:  &pathPrefixType,
				Value: ptrString("/api/"),
			},
			expectedNil:    false,
			expectedPrefix: "/api/",
		},
		{
			name: "exact path match",
			pathMatch: &gatewayv1.HTTPPathMatch{
				Type:  &exactType,
				Value: ptrString("/health"),
			},
			expectedNil:   false,
			expectedExact: "/health",
		},
		{
			name: "regex path match",
			pathMatch: &gatewayv1.HTTPPathMatch{
				Type:  &regexType,
				Value: ptrString("/v[0-9]+/.*"),
			},
			expectedNil:   false,
			expectedRegex: "/v[0-9]+/.*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertPathMatch(tt.pathMatch)

			if tt.expectedNil {
				if result != nil {
					t.Errorf("Expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("Expected non-nil result, got nil")
			}

			if tt.expectedPrefix != "" {
				if result.Prefix == nil || *result.Prefix != tt.expectedPrefix {
					t.Errorf("Expected prefix %s, got %v", tt.expectedPrefix, result.Prefix)
				}
			}
			if tt.expectedExact != "" {
				if result.Exact == nil || *result.Exact != tt.expectedExact {
					t.Errorf("Expected exact %s, got %v", tt.expectedExact, result.Exact)
				}
			}
			if tt.expectedRegex != "" {
				if result.Regex == nil || *result.Regex != tt.expectedRegex {
					t.Errorf("Expected regex %s, got %v", tt.expectedRegex, result.Regex)
				}
			}
		})
	}
}

func TestIsTranslatedResource(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			expected:    false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			expected:    false,
		},
		{
			name: "without translated annotation",
			annotations: map[string]string{
				"some-other": "value",
			},
			expected: false,
		},
		{
			name: "with translated annotation",
			annotations: map[string]string{
				AnnotationTranslatedFrom: "default/test-pool",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsTranslatedResource(tt.annotations)
			if result != tt.expected {
				t.Errorf("IsTranslatedResource(%v) = %v, expected %v",
					tt.annotations, result, tt.expected)
			}
		})
	}
}

func TestGetTranslationMetadata(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectedNil bool
		expected    *TranslationMetadata
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			expectedNil: true,
		},
		{
			name: "without translated annotation",
			annotations: map[string]string{
				"some-other": "value",
			},
			expectedNil: true,
		},
		{
			name: "with all metadata",
			annotations: map[string]string{
				AnnotationTranslatedFrom:  "default/test-pool",
				AnnotationOriginKind:      OriginKindInferencePool,
				AnnotationOriginName:      "test-pool",
				AnnotationOriginNamespace: "default",
			},
			expectedNil: false,
			expected: &TranslationMetadata{
				OriginKind:      OriginKindInferencePool,
				OriginName:      "test-pool",
				OriginNamespace: "default",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetTranslationMetadata(tt.annotations)

			if tt.expectedNil {
				if result != nil {
					t.Errorf("Expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("Expected non-nil result, got nil")
			}

			if result.OriginKind != tt.expected.OriginKind {
				t.Errorf("Expected OriginKind %s, got %s", tt.expected.OriginKind, result.OriginKind)
			}
			if result.OriginName != tt.expected.OriginName {
				t.Errorf("Expected OriginName %s, got %s", tt.expected.OriginName, result.OriginName)
			}
			if result.OriginNamespace != tt.expected.OriginNamespace {
				t.Errorf("Expected OriginNamespace %s, got %s", tt.expected.OriginNamespace, result.OriginNamespace)
			}
		})
	}
}

func TestGetURLRewriteConfigs(t *testing.T) {
	tests := []struct {
		name           string
		annotations    map[string]string
		expectedNil    bool
		expectedLength int
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			expectedNil: true,
		},
		{
			name: "without url rewrite annotation",
			annotations: map[string]string{
				"other": "value",
			},
			expectedNil: true,
		},
		{
			name: "with valid url rewrite config",
			annotations: map[string]string{
				AnnotationURLRewrite: `[{"type":"ReplacePrefixMatch","replacePrefixMatch":"/v1/","matchedPrefix":"/api/"}]`,
			},
			expectedNil:    false,
			expectedLength: 1,
		},
		{
			name: "with invalid json",
			annotations: map[string]string{
				AnnotationURLRewrite: `invalid json`,
			},
			expectedNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetURLRewriteConfigs(tt.annotations)

			if tt.expectedNil {
				if result != nil {
					t.Errorf("Expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("Expected non-nil result, got nil")
			}

			if len(result) != tt.expectedLength {
				t.Errorf("Expected length %d, got %d", tt.expectedLength, len(result))
			}
		})
	}
}

// Helper function to create string pointer
func ptrString(s string) *string {
	return &s
}
