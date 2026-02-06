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

const (
	// AnnotationTranslatedFrom indicates the original resource that this resource was translated from
	AnnotationTranslatedFrom = "kthena.volcano.sh/translated-from"
	// AnnotationOriginKind indicates the kind of the original resource
	AnnotationOriginKind = "kthena.volcano.sh/origin-kind"
	// AnnotationOriginName indicates the name of the original resource
	AnnotationOriginName = "kthena.volcano.sh/origin-name"
	// AnnotationOriginNamespace indicates the namespace of the original resource
	AnnotationOriginNamespace = "kthena.volcano.sh/origin-namespace"
	// AnnotationURLRewrite stores URL rewrite configuration for translated ModelRoutes
	AnnotationURLRewrite = "kthena.volcano.sh/url-rewrite"

	// OriginKindInferencePool is the kind for InferencePool
	OriginKindInferencePool = "InferencePool"
	// OriginKindHTTPRoute is the kind for HTTPRoute
	OriginKindHTTPRoute = "HTTPRoute"

	// TranslatedPrefix is the prefix used for translated resource names
	TranslatedPrefix = "translated"
)

// TranslationMetadata contains metadata about the original resource that was translated
type TranslationMetadata struct {
	// OriginKind is the kind of the original resource ("InferencePool" or "HTTPRoute")
	OriginKind string
	// OriginName is the name of the original resource
	OriginName string
	// OriginNamespace is the namespace of the original resource
	OriginNamespace string
}

// URLRewriteConfig stores URL rewrite configuration extracted from HTTPRoute filters
type URLRewriteConfig struct {
	// Type is the type of path modification
	Type string `json:"type,omitempty"`
	// ReplaceFullPath is the replacement for the full path
	ReplaceFullPath string `json:"replaceFullPath,omitempty"`
	// ReplacePrefixMatch is the replacement for the prefix match
	ReplacePrefixMatch string `json:"replacePrefixMatch,omitempty"`
	// MatchedPrefix is the prefix that was matched (for prefix replacement)
	MatchedPrefix string `json:"matchedPrefix,omitempty"`
	// Hostname is the replacement hostname
	Hostname string `json:"hostname,omitempty"`
}
