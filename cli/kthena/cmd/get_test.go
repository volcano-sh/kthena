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

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	networking "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestGetModelServingStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   string
	}{
		{
			name:       "NoConditions",
			conditions: []metav1.Condition{},
			expected:   "Unknown",
		},
		{
			name: "Available",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingAvailable),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Available",
		},
		{
			name: "Progressing",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingProgressing),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Progressing",
		},
		{
			name: "Updating",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingUpdateInProgress),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Updating",
		},
		{
			name: "AvailableTakesPriorityOverProgressing",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingProgressing),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelServingAvailable),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Available",
		},
		{
			name: "UpdateInProgressTakesPriorityOverProgressing",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingUpdateInProgress),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelServingProgressing),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Updating",
		},
		{
			name: "ConditionFalseIgnored",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingAvailable),
					Status: metav1.ConditionFalse,
				},
			},
			expected: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getModelServingStatus(tt.conditions)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetModelBoosterStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   string
	}{
		{
			name:       "NoConditions",
			conditions: []metav1.Condition{},
			expected:   "Unknown",
		},
		{
			name: "Active",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Active",
		},
		{
			name: "Failed",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeFailed),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Failed",
		},
		{
			name: "Initialized",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeInitialized),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Initialized",
		},
		{
			name: "FailedTakesPriorityOverActive",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelStatusConditionTypeFailed),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Failed",
		},
		{
			name: "ActiveTakesPriorityOverInitialized",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeInitialized),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Active",
		},
		{
			name: "ConditionFalseIgnored",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionFalse,
				},
			},
			expected: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getModelBoosterStatus(tt.conditions)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGetKthenaClient_HonorsKubeconfigEnv verifies the CLI resolves the
// kubeconfig the way kubectl does: a file named by KUBECONFIG wins over
// the home-directory default.
func TestGetKthenaClient_HonorsKubeconfigEnv(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6553
  name: env-cluster
contexts:
- context:
    cluster: env-cluster
    namespace: kubeconfig-env-namespace
    user: env-user
  name: env-context
current-context: env-context
users:
- name: env-user
  user: {}
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)

	_, contextNamespace, err := getKthenaClient()
	assert.NoError(t, err)
	assert.Equal(t, "kubeconfig-env-namespace", contextNamespace)
}

func TestValidateOutputFormat(t *testing.T) {
	tests := []struct {
		format  string
		wantErr bool
	}{
		{format: ""},
		{format: "table"},
		{format: "yaml"},
		{format: "json"},
		{format: "foo", wantErr: true},
		{format: "YAML", wantErr: true},
		{format: "yml", wantErr: true},
		{format: "wide", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			err := validateOutputFormat(tt.format)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestGetCmdRejectsUnsupportedOutputFormat verifies every get subcommand
// fails on an unknown -o value before it talks to the cluster.
func TestGetCmdRejectsUnsupportedOutputFormat(t *testing.T) {
	original := outputFormat
	t.Cleanup(func() { outputFormat = original })

	outputFormat = "foo"
	err := getCmd.PersistentPreRunE(getModelServersCmd, nil)
	assert.ErrorContains(t, err, `unsupported output format "foo"`)

	outputFormat = "json"
	assert.NoError(t, getCmd.PersistentPreRunE(getModelServersCmd, nil))
}

func TestPrintList(t *testing.T) {
	model := "qwen2.5-0.5b"
	server := networking.ModelServer{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.serving.volcano.sh/v1alpha1", Kind: "ModelServer"},
		ObjectMeta: metav1.ObjectMeta{
			Name:          "qwen-small",
			Namespace:     "default",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply"}},
		},
		Spec: networking.ModelServerSpec{Model: &model, InferenceEngine: "vLLM"},
	}

	tests := []struct {
		name      string
		format    string
		items     []networking.ModelServer
		wantNames []string
	}{
		{name: "yaml", format: "yaml", items: []networking.ModelServer{server}, wantNames: []string{"qwen-small"}},
		{name: "json", format: "json", items: []networking.ModelServer{server}, wantNames: []string{"qwen-small"}},
		{name: "yaml empty list", format: "yaml", items: []networking.ModelServer{}, wantNames: []string{}},
		{name: "json nil list", format: "json", items: nil, wantNames: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, printList(&buf, tt.format, tt.items))

			var got struct {
				APIVersion string                   `json:"apiVersion"`
				Kind       string                   `json:"kind"`
				Items      []networking.ModelServer `json:"items"`
			}
			if tt.format == "json" {
				require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
			} else {
				require.NoError(t, yaml.Unmarshal(buf.Bytes(), &got))
			}

			assert.Equal(t, "v1", got.APIVersion)
			assert.Equal(t, "List", got.Kind)
			require.NotNil(t, got.Items, "items should be an empty list, not null")
			names := []string{}
			for _, item := range got.Items {
				names = append(names, item.Name)
				assert.Equal(t, "ModelServer", item.Kind)
				assert.Equal(t, "qwen2.5-0.5b", *item.Spec.Model)
				assert.Empty(t, item.ManagedFields, "managedFields should be hidden like kubectl")
			}
			assert.Equal(t, tt.wantNames, names)
		})
	}
}

func TestPrintObjectUnsupportedFormat(t *testing.T) {
	var buf bytes.Buffer
	err := printObject(&buf, "table", ManifestInfo{Name: "x"})
	assert.Error(t, err)
	assert.Empty(t, buf.String())
}

func TestPrintObjectTemplateInfo(t *testing.T) {
	info := ManifestInfo{Name: "Qwen/Qwen3-32B", Description: "Qwen3 32B", FilePath: "Qwen/Qwen3-32B.yaml"}

	var buf bytes.Buffer
	require.NoError(t, printObject(&buf, "json", info))
	var got ManifestInfo
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, info, got)
}

// TestGetTemplateRejectsJSON verifies get template refuses -o json before it
// reads the template, since the template has no json form.
func TestGetTemplateRejectsJSON(t *testing.T) {
	original := outputFormat
	t.Cleanup(func() { outputFormat = original })

	outputFormat = "json"
	err := runGetTemplate(nil, []string{"Qwen/Qwen3-32B"})
	assert.ErrorContains(t, err, "-o json is not supported for get template")
}
