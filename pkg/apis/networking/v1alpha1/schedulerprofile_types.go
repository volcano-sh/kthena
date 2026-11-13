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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=schedulerprofiles,scope=Namespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SchedulerProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SchedulerProfileSpec `json:"spec"`
}

// SchedulerProfileList contains a list of SchedulerProfile.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SchedulerProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SchedulerProfile `json:"items"`
}

// SchedulerProfileSpec defines a complete scheduler plugin combination.
// The structure mirrors the router's global scheduler configuration
// (pluginConfig + plugins), but note the key casing differs: this API uses
// lowerCamelCase JSON names ("filter", "score"), the ConfigMap uses
// capitalized keys ("Filter", "Score").
type SchedulerProfileSpec struct {
	// PluginConfig holds per-plugin arguments, keyed by plugin name.
	// +optional
	PluginConfig []PluginConfig `json:"pluginConfig,omitempty"`
	// Plugins declares which filter and score plugins are enabled, with
	// their score weights. An empty section means no plugins of that kind
	// are enabled by this profile.
	// +optional
	Plugins Plugins `json:"plugins,omitempty"`
}

// PluginConfig associates configuration arguments with a named plugin.
type PluginConfig struct {
	// Name is the plugin the arguments apply to, e.g. "least-request".
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Args is the plugin-specific configuration in YAML/JSON form. The
	// accepted shape depends on the plugin (see each plugin's docs).
	// +optional
	Args runtime.RawExtension `json:"args,omitempty"`
}

// Plugins declares the enabled filter and score plugins of a profile.
// JSON names follow the Kubernetes API convention (lowerCamelCase); the
// router ConfigMap keeps its capitalized keys.
type Plugins struct {
	// Filter declares the filter plugins enabled by this profile.
	// +optional
	Filter Filter `json:"filter,omitempty"`
	// Score declares the score plugins enabled by this profile, each with
	// its weight in the final score sum.
	// +optional
	Score Score `json:"score,omitempty"`
}

// Filter lists the filter plugins enabled by a profile.
type Filter struct {
	// Enabled filter plugin names, executed in list order.
	// +optional
	Enabled []string `json:"enabled,omitempty"`
}

// Score lists the score plugins enabled by a profile with their weights.
type Score struct {
	// Enabled score plugins with weights. Weight must be greater than 0.
	// +optional
	Enabled []PluginWithWeight `json:"enabled,omitempty"`
}

// PluginWithWeight pairs a score plugin name with its weight.
type PluginWithWeight struct {
	// Name of the score plugin, e.g. "prefix-cache".
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Weight multiplies this plugin's score before it is summed with the
	// other plugins' weighted scores. Must be greater than 0.
	// +kubebuilder:validation:Minimum=1
	Weight int32 `json:"weight"`
}
