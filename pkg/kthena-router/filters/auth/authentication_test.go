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

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"github.com/stretchr/testify/assert"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestExtractTokenFromHeader(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{
			name:     "valid bearer token",
			header:   "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			expected: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "no bearer prefix",
			header:   "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			expected: "",
		},
		{
			name:     "empty header",
			header:   "",
			expected: "",
		},
		{
			name:     "bearer with space",
			header:   "Bearer ",
			expected: "",
		},
		{
			name:     "lowercase bearer scheme",
			header:   "bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			expected: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "bearer token with extra whitespace",
			header:   "  Bearer   eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9  ",
			expected: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "token with embedded whitespace",
			header:   "Bearer token with space",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Authorization", tt.header)

			result := extractTokenFromHeader(req)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNewJWTAuthenticatorConfig(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		validator := NewJWTAuthenticator(nil)
		assert.NotNil(t, validator)
		assert.False(t, validator.IsEnabled())
	})

	t.Run("empty JWKS URI", func(t *testing.T) {
		config := &conf.RouterConfiguration{
			Auth: conf.AuthenticationConfig{
				JwksUri: "",
				Issuer:  "test-issuer",
			},
		}
		validator := NewJWTAuthenticator(config)
		assert.NotNil(t, validator)
		assert.False(t, validator.IsEnabled())
	})

	t.Run("invalid JWKS URI", func(t *testing.T) {
		config := &conf.RouterConfiguration{
			Auth: conf.AuthenticationConfig{
				JwksUri: "invalid-url",
				Issuer:  "test-issuer",
			},
		}
		validator := NewJWTAuthenticator(config)
		assert.NotNil(t, validator)
		// The validator is enabled even with invalid URI, but will fail during actual validation
		assert.True(t, validator.IsEnabled())
		// Clean up the validator
		validator.Close()
	})
}

func TestJWTAuthenticatorIsEnabled(t *testing.T) {
	t.Run("enabled validator", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: true}
		assert.True(t, validator.IsEnabled())
	})

	t.Run("disabled validator", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: false}
		assert.False(t, validator.IsEnabled())
	})
}

func TestJWTAuthenticatorValidateToken(t *testing.T) {
	t.Run("disabled validator", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: false}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())

		err := validator.ValidateToken(context.Background(), c, "some-token")
		assert.NoError(t, err)
	})

	t.Run("empty token", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: true}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())

		err := validator.ValidateToken(context.Background(), c, "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "authorization header missing")
	})
}

func TestJWTAuthenticatorMiddleware(t *testing.T) {
	t.Run("disabled authenticator", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: false}
		middleware := validator.Authenticate()

		// Create test request
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)

		// Test that middleware passes through when disabled
		middleware(c)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("enabled authenticator without token", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: true}
		middleware := validator.Authenticate()

		// Create test request without authorization header
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)

		// Test that middleware returns 401 when no token provided
		middleware(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("enabled authenticator with empty token", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: true}
		middleware := validator.Authenticate()

		// Create test request with empty authorization header
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.Header.Set("Authorization", "Bearer ")

		// Test that middleware returns 401 when empty token provided
		middleware(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("enabled authenticator with invalid authorization scheme", func(t *testing.T) {
		validator := &JWTAuthenticator{enabled: true}
		middleware := validator.Authenticate()

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.Header.Set("Authorization", "Basic some-token")

		middleware(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestValidateAudiences(t *testing.T) {
	authenticator := &JWTAuthenticator{}
	token := jwt.New()

	t.Run("skip validation when no audiences configured", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{},
		}

		err := authenticator.validateAudiences(token, jwks)
		assert.NoError(t, err, "Should skip validation when no audiences configured")
	})

	t.Run("skip validation when no audiences configured and JWT have audience", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{},
		}
		token.Set("aud", "expected-audience")

		err := authenticator.validateAudiences(token, jwks)
		assert.NoError(t, err, "Should skip validation when no audiences configured")
		token.Remove("aud")
	})

	t.Run("missing audience claim in token", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{"expected-audience"},
		}

		err := authenticator.validateAudiences(token, jwks)
		assert.Error(t, err, "Should return error when audience claim is missing")
		assert.Contains(t, err.Error(), "audience claim missing", "Error message should indicate missing audience claim")
	})

	t.Run("nil audience value", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{"expected-audience"},
		}

		token.Set("aud", nil)

		err := authenticator.validateAudiences(token, jwks)
		assert.Error(t, err, "Should return error when audience is nil")
		assert.Contains(t, err.Error(), "audience claim missing", "Error message should indicate need for audience")
	})

	t.Run("single audience match", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{"expected-audience", "another-audience"},
		}

		token.Set("aud", "expected-audience")

		err := authenticator.validateAudiences(token, jwks)
		assert.NoError(t, err, "Should pass validation when single audience matches")
	})

	t.Run("single audience mismatch", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{"expected-audience", "another-audience"},
		}
		token.Set("aud", "different-audience")

		err := authenticator.validateAudiences(token, jwks)
		assert.Error(t, err, "Should return error when single audience does not match")
		assert.Contains(t, err.Error(), "audience mismatch", "Error message should indicate audience mismatch")
	})

	t.Run("multiple audiences match", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{"expected-audience", "another-audience"},
		}
		token.Set("aud", []string{"different-audience", "expected-audience"})

		err := authenticator.validateAudiences(token, jwks)
		assert.NoError(t, err, "Should pass validation when one of multiple audiences matches")
	})

	t.Run("multiple audiences mismatch", func(t *testing.T) {
		jwks := &Jwks{
			Audiences: []string{"expected-audience", "another-audience"},
		}
		token.Set("aud", []string{"different-audience", "yet-another-audience"})

		err := authenticator.validateAudiences(token, jwks)
		assert.Error(t, err, "Should return error when none of multiple audiences match")
		assert.Contains(t, err.Error(), "audience mismatch", "Error message should indicate audience mismatch")
	})

	token.Remove("aud")
}
