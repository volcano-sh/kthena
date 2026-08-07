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

package batch

import (
	"os"
	"strconv"
	"time"

	"k8s.io/klog/v2"
)

// Config holds runtime settings for the Files API storage backend.
// Empty FilesDir disables the API (store is not constructed).
type Config struct {
	FilesDir     string
	MaxFileBytes int64
	BatchTTL     time.Duration
}

// Enabled reports whether file storage is configured.
func (c Config) Enabled() bool {
	return c.FilesDir != ""
}

// LoadConfigFromEnv reads batch file settings from the environment.
// Missing or invalid values fall back to named defaults (same pattern as
// fairness / access-log env parsing in the router).
func LoadConfigFromEnv() Config {
	cfg := Config{
		FilesDir:     os.Getenv(EnvFilesDir),
		MaxFileBytes: DefaultMaxFileBytes,
		BatchTTL:     DefaultBatchTTL,
	}

	if s, ok := os.LookupEnv(EnvMaxFileBytes); ok && s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			klog.Warningf("Invalid %s %q, using default %d", EnvMaxFileBytes, s, DefaultMaxFileBytes)
		} else {
			cfg.MaxFileBytes = v
		}
	}

	if s, ok := os.LookupEnv(EnvBatchTTL); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			klog.Warningf("Invalid %s %q, using default %v", EnvBatchTTL, s, DefaultBatchTTL)
		} else {
			cfg.BatchTTL = d
		}
	}

	return cfg
}
