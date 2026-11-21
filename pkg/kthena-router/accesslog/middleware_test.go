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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

type recordingLogger struct {
	entries []*AccessLogEntry
}

func (l *recordingLogger) Log(entry *AccessLogEntry) error {
	l.entries = append(l.entries, entry)
	return nil
}

func (l *recordingLogger) Close() error { return nil }

func TestAccessLogMiddleware_ClaimFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	claimFields := map[string]string{"user_id": "sub", "tenant": "tid"}

	tests := []struct {
		name string
		// withExtractor is false when no claim field is configured.
		withExtractor bool
		// authHeader selects what the stand-in auth middleware does: "" leaves
		// JWT authentication disabled, "invalid" rejects the request and any
		// other value is a validated token payload.
		authHeader  string
		wantStatus  int
		wantFields  []ClaimField
		wantMissing []string
	}{
		{
			name:          "validated token",
			withExtractor: true,
			authHeader:    `{"sub":"u-1024","tid":"team-search"}`,
			wantStatus:    http.StatusOK,
			wantFields:    []ClaimField{{Name: "tenant", Value: "team-search"}, {Name: "user_id", Value: "u-1024"}},
		},
		{
			name:          "validated token without a mapped claim",
			withExtractor: true,
			authHeader:    `{"sub":"u-1024"}`,
			wantStatus:    http.StatusOK,
			wantFields:    []ClaimField{{Name: "user_id", Value: "u-1024"}},
			wantMissing:   []string{"tenant"},
		},
		{
			name:          "JWT authentication disabled",
			withExtractor: true,
			wantStatus:    http.StatusOK,
		},
		{
			// A rejected token is neither extracted nor counted.
			name:          "rejected token",
			withExtractor: true,
			authHeader:    "invalid",
			wantStatus:    http.StatusUnauthorized,
		},
		{
			name:       "no claim fields configured",
			authHeader: `{"sub":"u-1024","tid":"team-search"}`,
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var missing []string
			var extractor *ClaimFieldExtractor
			if tt.withExtractor {
				extractor = NewClaimFieldExtractor(claimFields, func(field string) { missing = append(missing, field) })
			}
			logger := &recordingLogger{}

			engine := gin.New()
			// Same order as the router: access log, then auth.
			engine.Use(AccessLogMiddleware(logger, extractor))
			engine.Use(func(c *gin.Context) {
				switch header := c.GetHeader("Authorization"); header {
				case "":
				case "invalid":
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				default:
					c.Set(common.JWTTokenKey, tokenFromClaims(t, header))
				}
				c.Next()
			})
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				// Claims are read once after the request, so nothing the
				// handler does, such as retrying pods, counts them twice.
				assert.Nil(t, GetAccessLogContext(c).ClaimFields)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
			require.Len(t, logger.entries, 1)
			assert.Equal(t, tt.wantFields, logger.entries[0].ClaimFields)
			assert.Equal(t, tt.wantMissing, missing)
		})
	}
}
