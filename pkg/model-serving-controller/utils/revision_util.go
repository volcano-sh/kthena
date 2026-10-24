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
	"fmt"
	"hash"
	"hash/fnv"

	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/rand"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Revision calculates the revision of an object using FNV hashing.
func Revision(obj interface{}) string {
	hasher := fnv.New32()
	DeepHashObject(hasher, obj)
	return rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
}

// DeepHashObject writes specified object to hash using the spew library
// which follows pointers and prints actual values of the nested objects
// ensuring the hash does not change when a pointer changes.
func DeepHashObject(hasher hash.Hash, objectToWrite interface{}) {
	hasher.Reset()
	fmt.Fprintf(hasher, "%v", dump.ForHash(objectToWrite))
}

// removeRoleReplicasForRevision removes fields that do not change rendered pods when calculating modelServing revision hash.
func removeRoleReplicasForRevision(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.ModelServing {
	copy := ms.DeepCopy()
	for i := range copy.Spec.Template.Roles {
		copy.Spec.Template.Roles[i].Replicas = nil
		copy.Spec.Template.Roles[i].RollingUpdateConfiguration = workloadv1alpha1.RollingUpdateConfiguration{}
	}

	return copy
}

type modelServingRevisionInput struct {
	Roles           []workloadv1alpha1.Role
	NetworkTopology *workloadv1alpha1.NetworkTopology
	GangPolicy      *workloadv1alpha1.GangPolicy
}

// ModelServingRevision calculates the revision of a ModelServing object.
func ModelServingRevision(ms *workloadv1alpha1.ModelServing) string {
	template := removeRoleReplicasForRevision(ms).Spec.Template
	if !hasNetworkTopology(template.NetworkTopology) && !hasGangPolicy(template.GangPolicy) {
		return Revision(template.Roles)
	}
	input := modelServingRevisionInput{Roles: template.Roles}
	if hasNetworkTopology(template.NetworkTopology) {
		input.NetworkTopology = template.NetworkTopology
	}
	if hasGangPolicy(template.GangPolicy) {
		input.GangPolicy = template.GangPolicy
	}
	return Revision(input)
}

func hasNetworkTopology(topology *workloadv1alpha1.NetworkTopology) bool {
	return topology != nil && (topology.GroupPolicy != nil || topology.RolePolicy != nil)
}

func hasGangPolicy(policy *workloadv1alpha1.GangPolicy) bool {
	return policy != nil && len(policy.MinRoleReplicas) > 0
}

// removeRoleReplicasForRoleTemplateHash removes fields that do not change rendered pods when calculating role template hash.
func removeRoleReplicasForRoleTemplateHash(role workloadv1alpha1.Role) workloadv1alpha1.Role {
	copy := role
	copy.Replicas = nil
	copy.RollingUpdateConfiguration = workloadv1alpha1.RollingUpdateConfiguration{}
	return copy
}

// CalRoleTemplateHash calculates the revision hash for a Role template.
func CalRoleTemplateHash(role workloadv1alpha1.Role) string {
	copy := removeRoleReplicasForRoleTemplateHash(role)
	return Revision(copy)
}
