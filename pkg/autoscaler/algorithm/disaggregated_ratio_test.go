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

package algorithm

import (
	"reflect"
	"testing"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestEnforceRoleRatio(t *testing.T) {
	constraint := &workload.RoleRatioConstraint{
		NumeratorRole:   "prefill",
		DenominatorRole: "decode",
		MinRatio:        resource.MustParse("0.25"),
		MaxRatio:        resource.MustParse("1"),
	}
	bounds := map[string]ReplicaBounds{
		"prefill": {Min: 0, Max: 8},
		"decode":  {Min: 0, Max: 16},
	}

	tests := []struct {
		name         string
		replicas     map[string]int32
		bounds       map[string]ReplicaBounds
		constraint   *workload.RoleRatioConstraint
		wantReplicas map[string]int32
		wantAdjusted bool
		wantRatio    string
	}{
		{
			name:         "already within ratio",
			replicas:     map[string]int32{"prefill": 2, "decode": 4},
			bounds:       bounds,
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 2, "decode": 4},
			wantAdjusted: false,
			wantRatio:    "0.5",
		},
		{
			name:         "below min raises numerator first",
			replicas:     map[string]int32{"prefill": 1, "decode": 8},
			bounds:       bounds,
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 2, "decode": 8},
			wantAdjusted: true,
			wantRatio:    "0.25",
		},
		{
			name:         "above max raises denominator first",
			replicas:     map[string]int32{"prefill": 6, "decode": 2},
			bounds:       bounds,
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 6, "decode": 6},
			wantAdjusted: true,
			wantRatio:    "1",
		},
		{
			name:     "fallback saturates numerator before lowering denominator",
			replicas: map[string]int32{"prefill": 2, "decode": 20},
			bounds: map[string]ReplicaBounds{
				"prefill": {Min: 0, Max: 3},
				"decode":  {Min: 0, Max: 20},
			},
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 3, "decode": 12},
			wantAdjusted: true,
			wantRatio:    "0.25",
		},
		{
			name:         "zero numerator is raised to keep both roles available",
			replicas:     map[string]int32{"prefill": 0, "decode": 4},
			bounds:       bounds,
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 1, "decode": 4},
			wantAdjusted: true,
			wantRatio:    "0.25",
		},
		{
			name:         "zero denominator is raised before ratio repair",
			replicas:     map[string]int32{"prefill": 4, "decode": 0},
			bounds:       bounds,
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 4, "decode": 4},
			wantAdjusted: true,
			wantRatio:    "1",
		},
		{
			name:         "both zero preserves coupled scale to zero",
			replicas:     map[string]int32{"prefill": 0, "decode": 0},
			bounds:       bounds,
			constraint:   constraint,
			wantReplicas: map[string]int32{"prefill": 0, "decode": 0},
			wantAdjusted: false,
			wantRatio:    "",
		},
		{
			name:         "nil constraint only clamps",
			replicas:     map[string]int32{"prefill": 10, "decode": 4},
			bounds:       bounds,
			constraint:   nil,
			wantReplicas: map[string]int32{"prefill": 8, "decode": 4},
			wantAdjusted: false,
			wantRatio:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReplicas, gotAdjusted, gotRatio, err := EnforceRoleRatio(tt.replicas, tt.bounds, tt.constraint)
			if err != nil {
				t.Fatalf("EnforceRoleRatio error: %v", err)
			}
			if !reflect.DeepEqual(gotReplicas, tt.wantReplicas) {
				t.Fatalf("replicas = %#v, want %#v", gotReplicas, tt.wantReplicas)
			}
			if gotAdjusted != tt.wantAdjusted {
				t.Fatalf("adjusted = %v, want %v", gotAdjusted, tt.wantAdjusted)
			}
			if gotRatio != tt.wantRatio {
				t.Fatalf("ratio = %q, want %q", gotRatio, tt.wantRatio)
			}
		})
	}
}
