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

package sessionsticky

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestTTL(t *testing.T) {
	tests := []struct {
		name string
		spec *networkingv1alpha1.SessionSticky
		want time.Duration
	}{
		{
			name: "nil spec uses default",
			spec: nil,
			want: 5 * time.Minute,
		},
		{
			name: "custom seconds",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: ptr.To[int32](600)},
			want: 10 * time.Minute,
		},
		{
			name: "unset seconds uses default",
			spec: &networkingv1alpha1.SessionSticky{},
			want: 5 * time.Minute,
		},
		{
			name: "minimum value 1 is kept",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: ptr.To[int32](1)},
			want: time.Second,
		},
		{
			name: "zero falls back to default",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: ptr.To[int32](0)},
			want: 5 * time.Minute,
		},
		{
			name: "negative falls back to default",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: ptr.To[int32](-5)},
			want: 5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TTL(tt.spec); got != tt.want {
				t.Errorf("TTL() = %v, want %v", got, tt.want)
			}
		})
	}
}
