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

// Package discovery separates registry-specific discovery from Store reconciliation.
package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// Source returns complete snapshots of routable endpoints. An error means the
// membership is unknown, while a successful empty snapshot removes all endpoints.
// Snapshot must honor cancellation. Close releases only this subscription and
// must finish promptly. The controller serializes Snapshot and Close per source.
// Returned snapshots must not be mutated by the provider after return.
type Source interface {
	Snapshot(context.Context) ([]api.Endpoint, error)
	Close()
}

// Factory creates a source without performing blocking discovery I/O. notify is
// a nonblocking refresh hint, not a delta; periodic snapshots repair lost hints.
// On error, factories must release any allocated resources and return a nil source.
// Parameters are read-only. Providers never receive a Store. Names identify
// instances within a ModelServer and may remain stable across address/port changes.
// Keeping a name preserves name-keyed prefix-cache history and session-sticky
// bindings. Use a new name when a replacement must not inherit this affinity state.
type Factory func(target types.NamespacedName, parameters map[string]interface{}, notify func()) (Source, error)

// Registry is supplied at construction. Implementations are compiled into the
// binary; there is no dynamic Go plugin loading or registration on the hot path.
type Registry map[string]Factory

// Config is the router's endpointDiscovery configuration, read at startup.
// Changes require a router restart; discovered endpoint changes do not.
type Config struct {
	// Provider selects one compiled-in factory for all mappings in this process.
	// Empty or "kubernetes" keeps native Pod/static discovery.
	Provider string `yaml:"provider"`
	// RefreshInterval is the periodic delay after a refresh, with jitter (default 30s).
	// Notifications can request an earlier refresh.
	RefreshInterval string `yaml:"refreshInterval"`
	// SnapshotTimeout bounds each Snapshot context (default 5s), not slot waiting,
	// source construction or Store reconciliation. Sources must honor cancellation.
	SnapshotTimeout string `yaml:"snapshotTimeout"`
	// MaxConcurrentSnapshots bounds provider queries across mappings in this process.
	// Zero defaults to four; this is not an inference-request limit.
	MaxConcurrentSnapshots int `yaml:"maxConcurrentSnapshots"`
	// RequireInitialSnapshot defaults to true: existing mapped ModelServers must
	// apply a valid snapshot before Ready. The server latches this startup gate.
	RequireInitialSnapshot *bool `yaml:"requireInitialSnapshot"`
	// Mappings is required only in external mode. Each target uses the same Provider
	// with its own parameters and Source; duplicate targets are rejected.
	Mappings []Mapping `yaml:"mappings"`
}

// Mapping binds an existing ModelServer to a provider-managed endpoint source.
type Mapping struct {
	// ModelServer is a namespace/name resource key, not the model name in requests.
	// An absent resource is watched for creation; the mapping does not create it.
	ModelServer string `yaml:"modelServer"`
	// Parameters is passed unchanged to the factory; its schema and validation
	// belong to the provider. Omission passes nil.
	Parameters map[string]interface{} `yaml:"parameters"`
}

func (c Config) External() bool { return c.Provider != "" && c.Provider != "kubernetes" }

func (c Config) durations() (time.Duration, time.Duration, error) {
	interval, timeout := 30*time.Second, 5*time.Second
	for _, setting := range []struct {
		value string
		out   *time.Duration
	}{{c.RefreshInterval, &interval}, {c.SnapshotTimeout, &timeout}} {
		if setting.value == "" {
			continue
		}
		d, err := time.ParseDuration(setting.value)
		if err != nil || d <= 0 {
			return 0, 0, fmt.Errorf("invalid discovery duration %q", setting.value)
		}
		*setting.out = d
	}
	return interval, timeout, nil
}

func (c Config) Validate() error {
	if c.MaxConcurrentSnapshots < 0 {
		return fmt.Errorf("maxConcurrentSnapshots must not be negative")
	}
	if _, _, err := c.durations(); err != nil {
		return err
	}
	if !c.External() {
		if len(c.Mappings) != 0 {
			return fmt.Errorf("endpoint discovery mappings require an external provider")
		}
		return nil
	}
	if len(c.Mappings) == 0 {
		return fmt.Errorf("external endpoint discovery requires mappings")
	}
	seen := make(map[string]bool)
	for _, mapping := range c.Mappings {
		parts := strings.Split(mapping.ModelServer, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || seen[mapping.ModelServer] {
			return fmt.Errorf("invalid or duplicate ModelServer mapping %q", mapping.ModelServer)
		}
		seen[mapping.ModelServer] = true
	}
	return nil
}
