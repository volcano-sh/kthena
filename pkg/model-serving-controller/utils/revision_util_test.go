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

package utils

import (
	"hash/fnv"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/assert"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

var (
	nginxPodTemplate = workloadv1alpha1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}
)
var replicas int32 = 1

func TestHashModelInferRevision(t *testing.T) {
	role1 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}
	role2 := workloadv1alpha1.Role{
		Name:           "decode",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 2,
		WorkerTemplate: &nginxPodTemplate,
	}
	role3 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}

	hash1 := Revision(role1)
	hash2 := Revision(role2)
	hash3 := Revision(role3)

	if hash1 == hash2 {
		t.Errorf("Hash should be different for different objects, got %s and %s", hash1, hash3)
	}
	if hash1 != hash3 {
		t.Errorf("Hash should be equal for identical objects, got %s and %s", hash1, hash2)
	}
}

func TestDeepHashObject(t *testing.T) {
	hasher := fnv.New32()
	role1 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}
	DeepHashObject(hasher, role1)
	firstHash := hasher.Sum32()

	hasher.Reset()
	DeepHashObject(hasher, role1)
	secondHash := hasher.Sum32()

	if firstHash != secondHash {
		t.Errorf("DeepHashObject should produce the same hash for the same object, got %v and %v", firstHash, secondHash)
	}
}

func TestRemoveRoleReplicasForRoleRevision(t *testing.T) {
	replicas1 := int32(3)
	replicas2 := int32(5)

	tests := []struct {
		name              string
		input             workloadv1alpha1.Role
		expected          workloadv1alpha1.Role
		originalUnchanged bool
	}{
		{
			name: "role with non-nil replicas",
			input: workloadv1alpha1.Role{
				Name:           "test-role",
				Replicas:       &replicas1,
				WorkerReplicas: 2,
			},
			expected: workloadv1alpha1.Role{
				Name:           "test-role",
				Replicas:       nil,
				WorkerReplicas: 2,
			},
			originalUnchanged: true,
		},
		{
			name: "role with nil replicas",
			input: workloadv1alpha1.Role{
				Name:           "test-role-nil",
				Replicas:       nil,
				WorkerReplicas: 5,
			},
			expected: workloadv1alpha1.Role{
				Name:           "test-role-nil",
				Replicas:       nil,
				WorkerReplicas: 5,
			},
			originalUnchanged: false,
		},
		{
			name: "role with different replicas value",
			input: workloadv1alpha1.Role{
				Name:           "another-role",
				Replicas:       &replicas2,
				WorkerReplicas: 10,
			},
			expected: workloadv1alpha1.Role{
				Name:           "another-role",
				Replicas:       nil,
				WorkerReplicas: 10,
			},
			originalUnchanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalReplicas := tt.input.Replicas

			result := RemoveRoleReplicasForRoleRevision(tt.input)

			assert.Nil(t, result.Replicas, "Expected result.Replicas to be nil")
			assert.Equal(t, tt.expected.Name, result.Name, "Name should remain the same")
			assert.Equal(t, tt.expected.WorkerReplicas, result.WorkerReplicas, "WorkerReplicas should remain the same")
			assert.Equal(t, tt.expected, result)
			if tt.originalUnchanged {
				assert.Equal(t, originalReplicas, tt.input.Replicas, "Original input should not be modified")
				assert.NotNil(t, tt.input.Replicas, "Original input Replicas should not be nil")
			}
		})
	}
}
