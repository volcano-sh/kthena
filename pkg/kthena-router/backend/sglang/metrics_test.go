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

package sglang

import "testing"

func TestNewSglangEngine_UsesDefaultMetricPort(t *testing.T) {
	engine := NewSglangEngine()

	if engine.MetricPort != 30000 {
		t.Fatalf("expected default metric port 30000, got %d", engine.MetricPort)
	}
}

func TestNewSglangEngine_UsesConfiguredMetricPort(t *testing.T) {
	engine := NewSglangEngine(31000)

	if engine.MetricPort != 31000 {
		t.Fatalf("expected metric port 31000, got %d", engine.MetricPort)
	}
}

func TestNewSglangEngine_FallsBackToDefaultWhenConfiguredPortIsZero(t *testing.T) {
	engine := NewSglangEngine(0)

	if engine.MetricPort != 30000 {
		t.Fatalf("expected fallback metric port 30000, got %d", engine.MetricPort)
	}
}

func TestNewSglangEngine_FallsBackToDefaultWhenConfiguredPortIsOutOfRange(t *testing.T) {
	engine := NewSglangEngine(70000)

	if engine.MetricPort != 30000 {
		t.Fatalf("expected fallback metric port 30000 for out-of-range port, got %d", engine.MetricPort)
	}
}

func TestNewSglangEngine_PanicsWhenMultiplePortsProvided(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when multiple metricPort arguments are provided")
		}
	}()
	_ = NewSglangEngine(30000, 30001)
}
