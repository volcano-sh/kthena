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
	"bytes"
	"encoding/json"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// EqualRevision is the shared hash-plus-semantic comparison for both workload
// snapshots and Role projections. Inputs must have the same schema and be
// normalized by their projection builders. A hash collision is not equality,
// and a hash mismatch is not sufficient evidence to replace a running workload.
// Invalid JSON is an error, never evidence of a changed template.
func EqualRevision[T any](leftHash, rightHash string, left, right []byte) (bool, error) {
	if leftHash == rightHash && bytes.Equal(left, right) && json.Valid(left) {
		return true, nil
	}
	var lhs, rhs T
	if err := json.Unmarshal(left, &lhs); err != nil {
		return false, fmt.Errorf("decode observed revision: %w", err)
	}
	if err := json.Unmarshal(right, &rhs); err != nil {
		return false, fmt.Errorf("decode desired revision: %w", err)
	}
	return apiequality.Semantic.DeepEqual(lhs, rhs), nil
}

// EqualModelServingRevisions compares the same normalized inputs that are
// persisted in ControllerRevisions, including scheduler and plugin inputs.
func EqualModelServingRevisions(leftHash, rightHash string, left, right *workloadv1alpha1.ModelServing) (bool, error) {
	lhs, err := BuildRevisionData(left)
	if err != nil {
		return false, err
	}
	rhs, err := BuildRevisionData(right)
	if err != nil {
		return false, err
	}
	lhs, err = modelServingComparisonData(lhs)
	if err != nil {
		return false, err
	}
	rhs, err = modelServingComparisonData(rhs)
	if err != nil {
		return false, err
	}
	return EqualRevision[[]modelServingRoleRevisionProjection](leftHash, rightHash, lhs, rhs)
}

// EqualRoleRevisions compares only the revisioned inputs applicable to this
// Role, derived from whole-ModelServing snapshots, not separate Role history.
func EqualRoleRevisions(leftHash, rightHash string, left, right *workloadv1alpha1.ModelServing, roleName string) (bool, error) {
	lhs, err := BuildRoleRevisionData(left, roleName)
	if err != nil {
		return false, err
	}
	rhs, err := BuildRoleRevisionData(right, roleName)
	if err != nil {
		return false, err
	}
	return EqualRevision[modelServingRoleRevisionProjection](leftHash, rightHash, lhs, rhs)
}
