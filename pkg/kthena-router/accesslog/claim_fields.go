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
	"reflect"
	"regexp"
	"sort"
	"strings"
)

const (
	// MaxClaimFields is the maximum number of claim fields an access log
	// may carry.
	MaxClaimFields = 8
	// MaxClaimFieldNameBytes is the maximum length of a claim field name.
	MaxClaimFieldNameBytes = 64
	// MaxClaimNameBytes is the maximum length of a JWT claim name.
	MaxClaimNameBytes = 64
	// MaxClaimValueBytes is the maximum length, in UTF-8 bytes, of a claim
	// value written to the access log. Longer values are omitted, not
	// truncated.
	MaxClaimValueBytes = 256
)

var claimFieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// textOnlyFieldNames are keys of the text format that are not JSON fields.
var textOnlyFieldNames = []string{"tokens", "timings"}

// reservedFieldNames holds every field name the access log writes itself, in
// either format, whether or not the option that writes it is enabled.
var reservedFieldNames = buildReservedFieldNames()

func buildReservedFieldNames() map[string]struct{} {
	names := make(map[string]struct{})
	entryType := reflect.TypeOf(AccessLogEntry{})
	for i := 0; i < entryType.NumField(); i++ {
		name, _, _ := strings.Cut(entryType.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names[name] = struct{}{}
		}
	}
	for _, name := range textOnlyFieldNames {
		names[name] = struct{}{}
	}
	return names
}

// IsReservedFieldName reports whether name is a field the access log writes
// itself and so cannot be used as a claim field name.
func IsReservedFieldName(name string) bool {
	_, ok := reservedFieldNames[name]
	return ok
}

// ValidateClaimFields checks a claim field mapping, keyed by access log field
// name with the top-level JWT claim name as value. It returns an error naming
// the first offending field in sorted order, so the result does not depend on
// map iteration order.
func ValidateClaimFields(claimFields map[string]string) error {
	if len(claimFields) > MaxClaimFields {
		return fmt.Errorf("at most %d claim fields are allowed, got %d", MaxClaimFields, len(claimFields))
	}

	fields := make([]string, 0, len(claimFields))
	for field := range claimFields {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	for _, field := range fields {
		if len(field) > MaxClaimFieldNameBytes {
			return fmt.Errorf("claim field name %q is %d bytes, at most %d are allowed",
				field, len(field), MaxClaimFieldNameBytes)
		}
		if !claimFieldNamePattern.MatchString(field) {
			return fmt.Errorf("claim field name %q must match %s", field, claimFieldNamePattern)
		}
		if IsReservedFieldName(field) {
			return fmt.Errorf("claim field name %q is reserved by the access log", field)
		}

		claim := claimFields[field]
		if strings.TrimSpace(claim) == "" {
			return fmt.Errorf("claim field %q must name a JWT claim", field)
		}
		if strings.TrimSpace(claim) != claim {
			return fmt.Errorf("claim field %q names JWT claim %q, which has leading or trailing whitespace", field, claim)
		}
		if len(claim) > MaxClaimNameBytes {
			return fmt.Errorf("claim field %q names a %d-byte JWT claim, at most %d bytes are allowed",
				field, len(claim), MaxClaimNameBytes)
		}
	}
	return nil
}
