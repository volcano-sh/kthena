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

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetCurrentTimestamp(t *testing.T) {
	before := time.Now().UnixMilli()
	got := GetCurrentTimestamp()
	after := time.Now().UnixMilli()

	assert.GreaterOrEqual(t, got, before)
	assert.LessOrEqual(t, got, after)
}

func TestSecondToTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		seconds int64
		wantMs  int64
	}{
		{"zero seconds", 0, 0},
		{"one second", 1, 1000},
		{"one minute", 60, 60000},
		{"negative seconds", -5, -5000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantMs, SecondToTimestamp(tt.seconds))
		})
	}
}

func TestIsRequestSuccess(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"200 OK", 200, true},
		{"204 No Content", 204, true},
		{"299 upper boundary of 2xx", 299, true},
		{"199 just below 2xx", 199, false},
		{"300 Redirect", 300, false},
		{"404 Not Found", 404, false},
		{"500 Internal Server Error", 500, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsRequestSuccess(tt.statusCode))
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
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
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
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: false,
		},
		{
			name: "pending pod",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: false,
		},
		{
			name: "succeeded pod",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsPodFailed(tt.pod))
		})
	}
}

func TestExtractKeysToSet(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]int
		want  map[string]struct{}
	}{
		{
			name:  "empty map returns empty set",
			input: map[string]int{},
			want:  map[string]struct{}{},
		},
		{
			name:  "single entry",
			input: map[string]int{"a": 1},
			want:  map[string]struct{}{"a": {}},
		},
		{
			name:  "multiple entries",
			input: map[string]int{"x": 10, "y": 20, "z": 30},
			want:  map[string]struct{}{"x": {}, "y": {}, "z": {}},
		},
		{
			name:  "values are ignored",
			input: map[string]int{"k": 0, "m": -1},
			want:  map[string]struct{}{"k": {}, "m": {}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractKeysToSet(tt.input))
		})
	}
}
