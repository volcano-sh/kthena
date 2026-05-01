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

// Hand-written DeepCopy implementations for the Phase 1 PD-aware
// coordination types.
//
// IMPORTANT (reviewers): zz_generated.deepcopy.go is intentionally left
// untouched. Before merging, run `make generate` so controller-gen
// regenerates that file — this will (a) duplicate the methods below into
// zz_generated.deepcopy.go and (b) extend HeterogeneousTarget.DeepCopyInto
// to deep-copy the new Coordination pointer. After regeneration, this file
// can be deleted.

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *Coordination) DeepCopyInto(out *Coordination) {
	*out = *in
	if in.PreferredRatio != nil {
		in, out := &in.PreferredRatio, &out.PreferredRatio
		*out = make(map[string]RoleRange, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
}

// DeepCopy returns a deep copy of the receiver, or nil if the receiver is nil.
func (in *Coordination) DeepCopy() *Coordination {
	if in == nil {
		return nil
	}
	out := new(Coordination)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
// RoleRange has no pointer fields, so a value copy suffices.
func (in *RoleRange) DeepCopyInto(out *RoleRange) { *out = *in }

// DeepCopy returns a deep copy of the receiver, or nil if the receiver is nil.
func (in *RoleRange) DeepCopy() *RoleRange {
	if in == nil {
		return nil
	}
	out := new(RoleRange)
	in.DeepCopyInto(out)
	return out
}
