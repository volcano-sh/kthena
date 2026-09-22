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
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

// serializedRevision hashes the JSON representation of an API object. Kubernetes
// ControllerRevision users hash serialized revision data instead of the Go
// struct itself, so newly added optional fields that are nil and omitted from
// JSON do not change the hash.
//
// The fallback preserves controller availability if a future API field cannot
// be JSON encoded. Role is a Kubernetes API type and is expected to remain JSON
// serializable, so the fallback is only a defensive guard.
func serializedRevision(obj interface{}) string {
	data, err := json.Marshal(obj)
	if err != nil {
		return Revision(obj)
	}
	return RevisionDataHash(data, nil)
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

// ModelServingRevision calculates the revision of a ModelServing object.
func ModelServingRevision(ms *workloadv1alpha1.ModelServing) string {
	data, err := BuildRevisionData(ms)
	if err != nil {
		return ""
	}
	return RevisionDataHash(data, modelServingCollisionCount(ms))
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
	return serializedRevision(copy)
}

// EqualRoleTemplatesForRevision compares legacy Roles-only revision data.
// The live controller uses EqualModelServingRevisions and EqualRoleRevisions,
// which also account for scheduler/plugins through the shared comparator.
func EqualRoleTemplatesForRevision(left, right []workloadv1alpha1.Role) bool {
	if len(left) != len(right) {
		return false
	}

	leftCopy := make([]workloadv1alpha1.Role, len(left))
	rightCopy := make([]workloadv1alpha1.Role, len(right))
	for i := range left {
		leftCopy[i] = removeRoleReplicasForRoleTemplateHash(*left[i].DeepCopy())
		rightCopy[i] = removeRoleReplicasForRoleTemplateHash(*right[i].DeepCopy())
	}
	return apiequality.Semantic.DeepEqual(leftCopy, rightCopy)
}

// EqualRoleTemplateForRevision is the single-Role legacy compatibility helper.
func EqualRoleTemplateForRevision(left, right workloadv1alpha1.Role) bool {
	return apiequality.Semantic.DeepEqual(
		removeRoleReplicasForRoleTemplateHash(*left.DeepCopy()),
		removeRoleReplicasForRoleTemplateHash(*right.DeepCopy()),
	)
}
