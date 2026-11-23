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

package plugins

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestBuildContextIsGlobal(t *testing.T) {
	if !(BuildContext{}).IsGlobal() {
		t.Error("a zero profile must report the global context")
	}
	if (BuildContext{Profile: types.NamespacedName{Namespace: "default", Name: "p1"}}).IsGlobal() {
		t.Error("a named profile must not report the global context")
	}
}

func TestGetPlugin(t *testing.T) {
	for _, name := range []string{
		GPUCacheUsagePluginName,
		LeastLatencyPluginName,
		LeastRequestPluginName,
		RandomPluginName,
		PrefixCachePluginName,
		KVCacheAwarePluginName,
		LoraAffinityPluginName,
	} {
		if _, ok := GetPlugin(name); !ok {
			t.Errorf("expected built-in plugin %q to be registered", name)
		}
	}
	if _, ok := GetPlugin("no-such-plugin"); ok {
		t.Error("an unknown plugin must not be returned")
	}
}

func TestSupportsRoles(t *testing.T) {
	tests := []struct {
		name          string
		filter, score bool
	}{
		{name: LeastRequestPluginName, filter: true, score: true},
		{name: GPUCacheUsagePluginName, score: true},
		{name: LoraAffinityPluginName, filter: true},
		{name: "no-such-plugin"},
	}
	for _, tt := range tests {
		if got := SupportsFilterRole(tt.name); got != tt.filter {
			t.Errorf("%q: SupportsFilterRole = %v, want %v", tt.name, got, tt.filter)
		}
		if got := SupportsScoreRole(tt.name); got != tt.score {
			t.Errorf("%q: SupportsScoreRole = %v, want %v", tt.name, got, tt.score)
		}
	}
}

func TestValidatePluginArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		wantErr string
	}{
		{name: LeastRequestPluginName, args: `{"maxWaitingRequests": 10}`},
		{name: LeastRequestPluginName, args: `{"maxWaitingRequests": -1}`, wantErr: "must not be negative"},
		{name: LeastLatencyPluginName, args: `{"TTFTTPOTWeightFactor": 0.5}`},
		{name: LeastLatencyPluginName, args: `{"TTFTTPOTWeightFactor": 1.1}`, wantErr: "must be a finite value"},
		{name: RandomPluginName, args: `{"x": 1}`, wantErr: "takes no arguments"},
		{name: "no-such-plugin", wantErr: "unknown plugin"},
	}
	for _, tt := range tests {
		err := ValidatePluginArgs(tt.name, runtime.RawExtension{Raw: []byte(tt.args)})
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%q with %q: expected no error, got %v", tt.name, tt.args, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q with %q: expected error %q, got none", tt.name, tt.args, tt.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%q with %q: expected error containing %q, got %q", tt.name, tt.args, tt.wantErr, err.Error())
		}
	}
}

func TestValidateScorePluginCombination(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		wantErr bool
	}{
		{name: "random alone is allowed", names: []string{RandomPluginName}},
		{name: "meaningful scores alone are allowed", names: []string{LeastRequestPluginName, GPUCacheUsagePluginName}},
		{name: "empty set is allowed", names: nil},
		{name: "random mixed with others is rejected", names: []string{RandomPluginName, LeastRequestPluginName}, wantErr: true},
	}
	for _, tt := range tests {
		err := ValidateScorePluginCombination(tt.names)
		if tt.wantErr && err == nil {
			t.Errorf("%s: expected an error, got none", tt.name)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("%s: expected no error, got %v", tt.name, err)
		}
	}
}

func TestRegisterPlugin(t *testing.T) {
	const fake = "test-fake-plugin"
	orig, hadOrig := GetPlugin(fake)
	defer func() {
		if hadOrig {
			RegisterPlugin(fake, orig)
		}
	}()

	RegisterPlugin(fake, SchedulerPlugin{
		ValidateArgs: func(runtime.RawExtension) error { return nil },
	})
	p, ok := GetPlugin(fake)
	if !ok || p.ValidateArgs == nil {
		t.Fatal("RegisterPlugin must make the plugin retrievable")
	}
	if IsKnownPlugin(fake) != true {
		t.Error("a registered plugin must be known")
	}

	// RegisterPlugin replaces the entry under the same name.
	RegisterPlugin(fake, SchedulerPlugin{})
	p, ok = GetPlugin(fake)
	if !ok {
		t.Fatal("a replaced plugin must remain retrievable")
	}
	if p.ValidateArgs != nil {
		t.Error("RegisterPlugin must replace the whole entry")
	}
}
