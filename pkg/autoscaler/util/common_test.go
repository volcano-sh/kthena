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

package util

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetCurrentTimestamp(t *testing.T) {
	before := time.Now().UnixMilli()
	got := GetCurrentTimestamp()
	after := time.Now().UnixMilli()

	if got < before || got > after {
		t.Errorf("GetCurrentTimestamp() = %d, want in range [%d, %d]", got, before, after)
	}
}

func TestSecondToTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		seconds int64
		wantMs  int64
	}{
		{
			name:    "zero seconds",
			seconds: 0,
			wantMs:  0,
		},
		{
			name:    "one second",
			seconds: 1,
			wantMs:  1000,
		},
		{
			name:    "one minute",
			seconds: 60,
			wantMs:  60000,
		},
		{
			name:    "negative seconds",
			seconds: -5,
			wantMs:  -5000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SecondToTimestamp(tt.seconds)
			if got != tt.wantMs {
				t.Errorf("SecondToTimestamp(%d) = %d, want %d", tt.seconds, got, tt.wantMs)
			}
		})
	}
}

func TestIsRequestSuccess(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{
			name:       "200 OK",
			statusCode: 200,
			want:       true,
		},
		{
			name:       "204 No Content",
			statusCode: 204,
			want:       true,
		},
		{
			name:       "299 upper boundary of 2xx",
			statusCode: 299,
			want:       true,
		},
		{
			name:       "199 just below 2xx",
			statusCode: 199,
			want:       false,
		},
		{
			name:       "300 Redirect",
			statusCode: 300,
			want:       false,
		},
		{
			name:       "404 Not Found",
			statusCode: 404,
			want:       false,
		},
		{
			name:       "500 Internal Server Error",
			statusCode: 500,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRequestSuccess(tt.statusCode)
			if got != tt.want {
				t.Errorf("IsRequestSuccess(%d) = %v, want %v", tt.statusCode, got, tt.want)
			}
		})
	}
}

func TestIsPodFailed(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "pod with failed phase",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodFailed},
			},
			want: true,
		},
		{
			name: "running pod with deletion timestamp set",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: true,
		},
		{
			name: "failed phase and deletion timestamp both set",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     corev1.PodStatus{Phase: corev1.PodFailed},
			},
			want: true,
		},
		{
			name: "running pod without deletion timestamp",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: false,
		},
		{
			name: "pending pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			want: false,
		},
		{
			name: "succeeded pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPodFailed(tt.pod)
			if got != tt.want {
				t.Errorf("IsPodFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractKeysToSet(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]int
		wantKeys []string
	}{
		{
			name:     "empty map returns empty set",
			input:    map[string]int{},
			wantKeys: []string{},
		},
		{
			name:     "single entry",
			input:    map[string]int{"a": 1},
			wantKeys: []string{"a"},
		},
		{
			name:     "multiple entries - all keys present",
			input:    map[string]int{"x": 10, "y": 20, "z": 30},
			wantKeys: []string{"x", "y", "z"},
		},
		{
			name:     "values are ignored - only keys matter",
			input:    map[string]int{"k": 0, "m": -1},
			wantKeys: []string{"k", "m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractKeysToSet(tt.input)

			if len(got) != len(tt.wantKeys) {
				t.Errorf("ExtractKeysToSet() len = %d, want %d", len(got), len(tt.wantKeys))
				return
			}

			for _, key := range tt.wantKeys {
				if _, ok := got[key]; !ok {
					t.Errorf("ExtractKeysToSet() missing key %q in result", key)
				}
			}

			// Verify no extra keys were added
			for key := range got {
				found := false
				for _, wantKey := range tt.wantKeys {
					if key == wantKey {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("ExtractKeysToSet() unexpected key %q in result", key)
				}
			}
		})
	}
}
