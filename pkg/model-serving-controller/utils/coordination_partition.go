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
	"math/big"
	"strconv"
	"strings"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// CoordinatedPartitionConflict finds the first selected Role whose partition
// cannot be obtained by rounding up a common nominal fraction of its replicas.
// It does not default or rewrite the supplied configuration.
func CoordinatedPartitionConflict(ms *api.ModelServing) (int, error) {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != api.RoleRollingUpdate || ms.Spec.RolloutStrategy.RoleCoordination == nil {
		return -1, nil
	}
	selected := map[string]bool{}
	for _, name := range ms.Spec.RolloutStrategy.RoleCoordination.Roles {
		selected[name] = true
	}
	lower, upper := new(big.Rat), big.NewRat(1, 1)
	lowerOpen := false
	for i, role := range ms.Spec.Template.Roles {
		if len(selected) > 0 && !selected[role.Name] {
			continue
		}
		n := int32(1)
		if role.Replicas != nil {
			n = *role.Replicas
		}
		p := 0
		if role.Partition != nil {
			var err error
			p, err = intstr.GetScaledValueFromIntOrPercent(role.Partition, int(n), true)
			if err != nil {
				return i, err
			}
		}
		if n < 0 || p < 0 || p > int(n) {
			return i, fmt.Errorf("partition must be within the Role replica count")
		}
		if n == 0 {
			continue
		}
		lo, hi, open := new(big.Rat), new(big.Rat), false
		if role.Partition != nil && role.Partition.Type == intstr.String {
			percent, err := strconv.Atoi(strings.TrimSuffix(role.Partition.StrVal, "%"))
			if err != nil || percent < 0 || percent > 100 || !strings.HasSuffix(role.Partition.StrVal, "%") {
				return i, fmt.Errorf("partition must be a whole percentage from 0%% to 100%%")
			}
			lo.SetFrac64(int64(percent), 100)
			hi.Set(lo)
		} else if p > 0 {
			lo.SetFrac64(int64(p-1), int64(n))
			hi.SetFrac64(int64(p), int64(n))
			open = true
		}
		if cmp := lo.Cmp(lower); cmp > 0 {
			lower.Set(lo)
			lowerOpen = open
		} else if cmp == 0 {
			lowerOpen = lowerOpen || open
		}
		if hi.Cmp(upper) < 0 {
			upper.Set(hi)
		}
		if cmp := lower.Cmp(upper); cmp > 0 || (cmp == 0 && lowerOpen) {
			return i, fmt.Errorf("coordinated Roles must use partitions compatible with one common nominal proportion (each Role rounds up independently); Role %q conflicts with the preceding selected Roles", role.Name)
		}
	}
	return -1, nil
}
