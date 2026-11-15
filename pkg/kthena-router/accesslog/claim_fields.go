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
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwt"
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

// ClaimField is one claim field written to an access log entry.
type ClaimField struct {
	// Name is the access log field name.
	Name string
	// Value is the claim value converted to a string.
	Value string
}

type claimMapping struct {
	field string
	claim string
}

// ClaimFieldExtractor reads the configured claim fields from a validated JWT.
type ClaimFieldExtractor struct {
	// mappings is sorted by field name, which is also the output order.
	mappings []claimMapping
	// onMissing is called with the field name when a token has no usable value
	// for the field's claim.
	onMissing func(field string)
}

// NewClaimFieldExtractor returns an extractor for claimFields, a mapping
// already checked by ValidateClaimFields, or nil when claimFields is empty.
// onMissing may be nil.
func NewClaimFieldExtractor(claimFields map[string]string, onMissing func(field string)) *ClaimFieldExtractor {
	if len(claimFields) == 0 {
		return nil
	}
	mappings := make([]claimMapping, 0, len(claimFields))
	for field, claim := range claimFields {
		mappings = append(mappings, claimMapping{field: field, claim: claim})
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].field < mappings[j].field })
	return &ClaimFieldExtractor{mappings: mappings, onMissing: onMissing}
}

// Extract returns the claim fields token has usable values for, in field name
// order, and reports every other configured field to onMissing.
func (e *ClaimFieldExtractor) Extract(token jwt.Token) []ClaimField {
	if e == nil || token == nil {
		return nil
	}
	var fields []ClaimField
	for _, m := range e.mappings {
		value, ok := claimValue(token, m.claim)
		if !ok {
			if e.onMissing != nil {
				e.onMissing(m.field)
			}
			continue
		}
		fields = append(fields, ClaimField{Name: m.field, Value: value})
	}
	return fields
}

// claimValue converts the top-level claim name of token to a log value.
// Strings are trimmed; numbers and booleans are formatted. A missing claim,
// null, an object, an array, an empty string and a value longer than
// MaxClaimValueBytes have no usable value; long values are not truncated.
//
// Unlike claimString in the auth filter, which session stickiness relies on,
// this never formats arbitrary values with fmt.
func claimValue(token jwt.Token, name string) (string, bool) {
	if !token.Has(name) {
		return "", false
	}
	var raw any
	if err := token.Get(name, &raw); err != nil {
		return "", false
	}

	var value string
	switch v := raw.(type) {
	case string:
		value = strings.TrimSpace(v)
	case bool:
		value = strconv.FormatBool(v)
	case float64:
		value = strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		value = strconv.Itoa(v)
	case int64:
		value = strconv.FormatInt(v, 10)
	case json.Number:
		value = v.String()
	case time.Time:
		// exp, iat and nbf are NumericDate values: seconds since the epoch.
		value = strconv.FormatInt(v.Unix(), 10)
	default:
		// nil, objects (map[string]any) and arrays ([]any, and the []string
		// of aud) are not written.
		return "", false
	}
	if value == "" || len(value) > MaxClaimValueBytes {
		return "", false
	}
	return value, true
}
