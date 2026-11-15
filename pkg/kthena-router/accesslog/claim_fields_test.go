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

package accesslog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateClaimFields(t *testing.T) {
	maxFields := make(map[string]string, MaxClaimFields)
	for i := 0; i < MaxClaimFields; i++ {
		maxFields[fmt.Sprintf("claim_%d", i)] = "sub"
	}
	tooManyFields := make(map[string]string, MaxClaimFields+1)
	for i := 0; i <= MaxClaimFields; i++ {
		tooManyFields[fmt.Sprintf("claim_%d", i)] = "sub"
	}
	longestFieldName := "f" + strings.Repeat("x", MaxClaimFieldNameBytes-1)
	longestClaimName := strings.Repeat("c", MaxClaimNameBytes)

	tests := []struct {
		name        string
		claimFields map[string]string
		wantErr     string
	}{
		{name: "nil mapping", claimFields: nil},
		{name: "empty mapping", claimFields: map[string]string{}},
		{
			name: "issuer templates",
			claimFields: map[string]string{
				"user_id":   "sub",
				"client_id": "azp",
				"tenant":    "https://example.com/tenant",
			},
		},
		{name: "digits and underscores", claimFields: map[string]string{"org_id_2": "org_id"}},
		{name: "same claim twice", claimFields: map[string]string{"user_id": "sub", "subject": "sub"}},
		{name: "maximum field count", claimFields: maxFields},
		{name: "maximum name lengths", claimFields: map[string]string{longestFieldName: longestClaimName}},
		{
			name:        "too many fields",
			claimFields: tooManyFields,
			wantErr:     "at most 8 claim fields are allowed, got 9",
		},
		{
			name:        "field name too long",
			claimFields: map[string]string{longestFieldName + "x": "sub"},
			wantErr:     fmt.Sprintf("claim field name %q is 65 bytes, at most 64 are allowed", longestFieldName+"x"),
		},
		{
			name:        "empty field name",
			claimFields: map[string]string{"": "sub"},
			wantErr:     `claim field name "" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "uppercase field name",
			claimFields: map[string]string{"userId": "sub"},
			wantErr:     `claim field name "userId" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "field name starting with a digit",
			claimFields: map[string]string{"1user": "sub"},
			wantErr:     `claim field name "1user" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "field name starting with an underscore",
			claimFields: map[string]string{"_user": "sub"},
			wantErr:     `claim field name "_user" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "field name with a hyphen",
			claimFields: map[string]string{"user-id": "sub"},
			wantErr:     `claim field name "user-id" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "field name with a space",
			claimFields: map[string]string{"user id": "sub"},
			wantErr:     `claim field name "user id" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "field name with an equals sign",
			claimFields: map[string]string{"user=id": "sub"},
			wantErr:     `claim field name "user=id" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "field name with a trailing newline",
			claimFields: map[string]string{"user\n": "sub"},
			wantErr:     `claim field name "user\n" must match ^[a-z][a-z0-9_]*$`,
		},
		{
			name:        "reserved JSON field",
			claimFields: map[string]string{"request_id": "jti"},
			wantErr:     `claim field name "request_id" is reserved by the access log`,
		},
		{
			name:        "reserved text field",
			claimFields: map[string]string{"tokens": "sub"},
			wantErr:     `claim field name "tokens" is reserved by the access log`,
		},
		{
			name:        "reserved upstream usage field",
			claimFields: map[string]string{"cached_input_tokens": "sub"},
			wantErr:     `claim field name "cached_input_tokens" is reserved by the access log`,
		},
		{
			name:        "empty claim name",
			claimFields: map[string]string{"user_id": ""},
			wantErr:     `claim field "user_id" must name a JWT claim`,
		},
		{
			name:        "blank claim name",
			claimFields: map[string]string{"user_id": "  "},
			wantErr:     `claim field "user_id" must name a JWT claim`,
		},
		{
			name:        "claim name with surrounding whitespace",
			claimFields: map[string]string{"user_id": " sub"},
			wantErr:     `claim field "user_id" names JWT claim " sub", which has leading or trailing whitespace`,
		},
		{
			name:        "claim name too long",
			claimFields: map[string]string{"user_id": longestClaimName + "c"},
			wantErr:     `claim field "user_id" names a 65-byte JWT claim, at most 64 bytes are allowed`,
		},
		{
			// Fields are checked in sorted order, so the error is stable.
			name:        "first invalid field in sorted order",
			claimFields: map[string]string{"zz": "", "aa": "", "mm": ""},
			wantErr:     `claim field "aa" must name a JWT claim`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateClaimFields(tt.claimFields)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

func TestIsReservedFieldName(t *testing.T) {
	// Every field either format writes is reserved, including fields behind
	// options that are off by default.
	reserved := []string{
		"timestamp", "method", "path", "protocol", "status_code", "error",
		"model_name", "model_route", "model_server", "selected_pod", "request_id",
		"backend_type", "backend_name", "upstream_model", "upstream_status_code",
		"upstream_attempts", "error_origin", "gateway", "http_route", "inference_pool",
		"input_tokens", "upstream_input_tokens", "cached_input_tokens", "output_tokens",
		"duration_total", "duration_request_processing", "duration_upstream_processing",
		"duration_response_processing", "tokens", "timings",
	}
	for _, name := range reserved {
		assert.True(t, IsReservedFieldName(name), "%q should be reserved", name)
	}
	assert.Len(t, reservedFieldNames, len(reserved), "reserved names changed; update this list")

	for _, name := range []string{"user_id", "tenant", "client_id", "type", "message"} {
		assert.False(t, IsReservedFieldName(name), "%q should not be reserved", name)
	}
}
