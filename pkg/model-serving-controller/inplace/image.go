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
	"github.com/distribution/reference"
)

// SameImage compares normalized references, so "nginx" equals "docker.io/library/nginx:latest".
// A reference with a digest matches the same name and digest, whatever its tag.
func SameImage(a, b string) bool {
	na, ok := normalizeImage(a)
	if !ok {
		return false
	}
	nb, ok := normalizeImage(b)
	if !ok {
		return false
	}
	return na == nb
}

func normalizeImage(image string) (string, bool) {
	if image == "" {
		return "", false
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", false
	}

	if digested, ok := named.(reference.Digested); ok {
		canonical, err := reference.WithDigest(reference.TrimNamed(named), digested.Digest())
		if err != nil {
			return "", false
		}
		return canonical.String(), true
	}
	return reference.TagNameOnly(named).String(), true
}
