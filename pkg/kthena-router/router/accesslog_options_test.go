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

package router

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestInactiveAccessLogOptions(t *testing.T) {
	const (
		upstreamUsageNote  = "accessLog.includeUpstreamUsage is set but access logging is disabled; upstream usage is not logged"
		claimsNoAccessNote = "accessLog.claimFields is set but access logging is disabled; claim fields are not logged"
		claimsNoJWTNote    = "accessLog.claimFields is set but JWT authentication is disabled (auth.jwksUri is empty); claim fields are not logged"
	)
	claims := map[string]string{"user_id": "sub"}

	tests := []struct {
		name             string
		cfg              conf.AccessLogConfig
		accessLogEnabled bool
		jwtEnabled       bool
		want             []string
	}{
		{name: "nothing configured", cfg: conf.AccessLogConfig{}, accessLogEnabled: true, jwtEnabled: true},
		{name: "nothing configured, everything disabled", cfg: conf.AccessLogConfig{}},
		{
			name:             "all options active",
			cfg:              conf.AccessLogConfig{IncludeUpstreamUsage: true, ClaimFields: claims},
			accessLogEnabled: true,
			jwtEnabled:       true,
		},
		{
			name:       "access logging disabled",
			cfg:        conf.AccessLogConfig{IncludeUpstreamUsage: true, ClaimFields: claims},
			jwtEnabled: true,
			want:       []string{upstreamUsageNote, claimsNoAccessNote},
		},
		{
			// Upstream usage does not depend on JWT authentication.
			name:             "JWT authentication disabled",
			cfg:              conf.AccessLogConfig{IncludeUpstreamUsage: true, ClaimFields: claims},
			accessLogEnabled: true,
			want:             []string{claimsNoJWTNote},
		},
		{
			name: "both disabled",
			cfg:  conf.AccessLogConfig{ClaimFields: claims},
			want: []string{claimsNoAccessNote, claimsNoJWTNote},
		},
		{
			name: "empty claim mapping",
			cfg:  conf.AccessLogConfig{ClaimFields: map[string]string{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, inactiveAccessLogOptions(tt.cfg, tt.accessLogEnabled, tt.jwtEnabled))
		})
	}
}

func TestNewClaimFieldExtractor(t *testing.T) {
	cfg := conf.AccessLogConfig{ClaimFields: map[string]string{"user_id": "sub"}}
	tests := []struct {
		name             string
		cfg              conf.AccessLogConfig
		accessLogEnabled bool
		jwtEnabled       bool
		wantExtractor    bool
	}{
		{name: "active", cfg: cfg, accessLogEnabled: true, jwtEnabled: true, wantExtractor: true},
		{name: "access logging disabled", cfg: cfg, jwtEnabled: true},
		{name: "JWT authentication disabled", cfg: cfg, accessLogEnabled: true},
		{name: "no claim fields", accessLogEnabled: true, jwtEnabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extractor := newClaimFieldExtractor(tt.cfg, tt.accessLogEnabled, tt.jwtEnabled, nil)
			assert.Equal(t, tt.wantExtractor, extractor != nil)
		})
	}
}
