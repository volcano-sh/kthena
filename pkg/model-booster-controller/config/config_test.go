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

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDownloaderImage(t *testing.T) {
	testCases := []struct {
		name            string
		downloaderImage string
		expectedImage   string
	}{
		{
			name:            "returns default when image is empty",
			downloaderImage: "",
			expectedImage:   DefaultDownloaderImage,
		},
		{
			name:            "returns custom image when set",
			downloaderImage: "my-registry/downloader:v1.0",
			expectedImage:   "my-registry/downloader:v1.0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ParseConfig{downloaderImage: tc.downloaderImage}
			assert.Equal(t, tc.expectedImage, p.DownloaderImage())
		})
	}
}

func TestSetDownloaderImage(t *testing.T) {
	p := &ParseConfig{}
	p.SetDownloaderImage("registry/downloader:latest")
	assert.Equal(t, "registry/downloader:latest", p.DownloaderImage())
}

func TestRuntimeImage(t *testing.T) {
	testCases := []struct {
		name          string
		runtimeImage  string
		expectedImage string
	}{
		{
			name:          "returns default when image is empty",
			runtimeImage:  "",
			expectedImage: DefaultRuntimeImage,
		},
		{
			name:          "returns custom image when set",
			runtimeImage:  "my-registry/runtime:v2.0",
			expectedImage: "my-registry/runtime:v2.0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ParseConfig{runtimeImage: tc.runtimeImage}
			assert.Equal(t, tc.expectedImage, p.RuntimeImage())
		})
	}
}

func TestSetRuntimeImage(t *testing.T) {
	p := &ParseConfig{}
	p.SetRuntimeImage("registry/runtime:latest")
	assert.Equal(t, "registry/runtime:latest", p.RuntimeImage())
}
