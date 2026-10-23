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

package inplace

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSameImage(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"nginx", "docker.io/library/nginx:latest", true},
		{"nginx:1.25", "docker.io/library/nginx:1.25", true},
		{"example.com/engine:v2", "example.com/engine:v2", true},
		{"example.com/engine:v2", "example.com/engine:v1", false},
		{"example.com/engine@sha256:" + sha, "example.com/engine@sha256:" + sha, true},
		{"example.com/engine:v2@sha256:" + sha, "example.com/engine@sha256:" + sha, true},
		{"example.com/engine:v2@sha256:" + sha, "example.com/engine:v1@sha256:" + sha, true},
		{"example.com/engine:v2@sha256:" + sha, "example.com/engine:v2", false},
		{"example.com/engine@sha256:" + sha, "example.com/other@sha256:" + sha, false},
		{"example.com/engine:v2", "", false},
		{"", "", false},
		{"Invalid:Image", "Invalid:Image", false},
	}
	for _, tt := range tests {
		assert.Equalf(t, tt.want, SameImage(tt.a, tt.b), "SameImage(%q, %q)", tt.a, tt.b)
	}
}

const sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
