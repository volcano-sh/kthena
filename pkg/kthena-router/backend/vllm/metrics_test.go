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

package vllm

import "testing"

func TestNewVllmEngine_UsesDefaultMetricPort(t *testing.T) {
	engine := NewVllmEngine()

	if engine.MetricPort != 8000 {
		t.Fatalf("expected default metric port 8000, got %d", engine.MetricPort)
	}
}

func TestNewVllmEngine_UsesConfiguredMetricPort(t *testing.T) {
	engine := NewVllmEngine(18000)

	if engine.MetricPort != 18000 {
		t.Fatalf("expected custom metric port 18000, got %d", engine.MetricPort)
	}
}

func TestNewVllmEngine_FallsBackToDefaultWhenConfiguredPortIsZero(t *testing.T) {
	engine := NewVllmEngine(0)

	if engine.MetricPort != 8000 {
		t.Fatalf("expected fallback metric port 8000, got %d", engine.MetricPort)
	}
}

func TestNewVllmEngine_FallsBackToDefaultWhenConfiguredPortIsOutOfRange(t *testing.T) {
	engine := NewVllmEngine(70000)

	if engine.MetricPort != 8000 {
		t.Fatalf("expected fallback metric port 8000 for out-of-range port, got %d", engine.MetricPort)
	}
}

func TestNewVllmEngine_PanicsWhenMultiplePortsProvided(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when multiple metricPort arguments are provided")
		}
	}()
	_ = NewVllmEngine(8000, 8001)
}
