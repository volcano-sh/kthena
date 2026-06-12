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

// Package auth provides JWT authentication and authorization functionality for the Kthena router.
// This package handles JWT token validation, JWKS rotation, and provides middleware for Gin HTTP framework.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

// JWT token extraction constants
const (
	header       = "Authorization"
	bearerScheme = "Bearer"
)

// extractTokenFromHeader extracts the Bearer token from the Authorization header
func extractTokenFromHeader(req *http.Request) string {
	fields := strings.Fields(req.Header.Get(header))
	if len(fields) != 2 || !strings.EqualFold(fields[0], bearerScheme) {
		return ""
	}
	return fields[1]
}

// JWTAuthenticator provides JWT token validation with automatic JWKS rotation support
type JWTAuthenticator struct {
	enabled bool         // Whether JWT authentication is enabled
	rotator *JWKSRotator // JWKS rotator for automatic key updates
}

// NewJWTAuthenticator creates a new JWTAuthenticator with JWKS rotation support
func NewJWTAuthenticator(routerConfig *conf.RouterConfiguration) *JWTAuthenticator {
	if routerConfig == nil || routerConfig.Auth.JwksUri == "" {
		klog.V(4).Info("JWKS URI not configured, authentication disabled")
		return &JWTAuthenticator{enabled: false}
	}

	// Create and configure the JWKS rotator
	rotator := NewJWKSRotator(routerConfig.Auth)
	if rotator != nil {
		rotator.Start(context.TODO())
	}

	return &JWTAuthenticator{
		enabled: true,
		rotator: rotator,
	}
}

// Close gracefully closes the JWTAuthenticator and its resources
func (j *JWTAuthenticator) Close() {
	if j.rotator != nil {
		j.rotator.Stop()
	}
}

// parseAndValidate parses and validates a JWT and returns the token and subject.
func (j *JWTAuthenticator) parseAndValidate(tokenStr string) (jwt.Token, string, error) {
	jwksValue := j.rotator.GetJwks()
	if jwksValue.Jwks == nil {
		return nil, "", fmt.Errorf("no JWKS available for token validation")
	}

	token, err := jwt.Parse([]byte(tokenStr), jwt.WithKeySet(jwksValue.Jwks, jws.WithInferAlgorithmFromKey(true)))
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse jwt: %w", err)
	}

	if err := j.validateClaims(token, jwksValue); err != nil {
		return nil, "", fmt.Errorf("failed to validate claims: %w", err)
	}

	sub, _ := token.Subject()
	return token, sub, nil
}

func (j *JWTAuthenticator) validateClaims(token jwt.Token, jwks *Jwks) error {
	if err := j.validateIssuer(token, jwks); err != nil {
		return fmt.Errorf("issuer validation failed: %w", err)
	}

	if err := j.validateAudiences(token, jwks); err != nil {
		return fmt.Errorf("audience validation failed: %w", err)
	}

	if err := j.validateTimeClaims(token); err != nil {
		return fmt.Errorf("time claims validation failed: %w", err)
	}

	return nil
}

func (j *JWTAuthenticator) validateIssuer(token jwt.Token, jwks *Jwks) error {
	var iss string
	if err := token.Get("iss", &iss); err != nil || iss != jwks.Issuer {
		return fmt.Errorf("invalid issuer: expected %s, got %v", jwks.Issuer, iss)
	}
	return nil
}

func (j *JWTAuthenticator) validateAudiences(token jwt.Token, jwks *Jwks) error {
	var aud interface{}
	audinecesCache := jwks.Audiences
	if len(audinecesCache) == 0 {
		// If audiences are not configured, we skip audience validation
		return nil
	}

	err := token.Get("aud", &aud)
	if err != nil {
		return fmt.Errorf("audience claim missing")
	}

	if aud == nil {
		return fmt.Errorf("need an audience")
	}

	// validate audience
	audMatched := false
	switch audVal := aud.(type) {
	case string:
		for _, expectedAud := range audinecesCache {
			if audVal == expectedAud {
				audMatched = true
				break
			}
		}
	case []string:
		for _, audItem := range audVal {
			for _, expectedAud := range audinecesCache {
				if audItem == expectedAud {
					audMatched = true
					break
				}
			}
		}
	}

	if !audMatched {
		return fmt.Errorf("audience mismatch: expected one of %v, got %v", audinecesCache, aud)
	}
	return nil
}

// validateTimeClaims validates the fields related to effective time (exp, nbf, iat)
// If token doesn't have exp, the token is invalid.
// For nbf and iat it's a little more lenient.
// If token doesn't have nbf, we assume it's valid.
// As same for iat.
func (j *JWTAuthenticator) validateTimeClaims(token jwt.Token) error {
	now := time.Now()
	var exp, nbf, iat interface{}
	// Validate Token expiration(exp)
	if err := token.Get("exp", &exp); err == nil {
		if err := j.validateExpiration(exp, now); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("expiration claim (exp) missing")
	}

	// validate Token not before (nbf)
	if err := token.Get("nbf", &nbf); err == nil {
		if err := j.validateNotBefore(nbf, now); err != nil {
			return err
		}
	}

	// validate Token issued at (iat)
	if err := token.Get("iat", &iat); err == nil {
		if err := j.validateIssuedAt(iat, now); err != nil {
			return err
		}
	}

	return nil
}

func (j *JWTAuthenticator) validateExpiration(exp interface{}, now time.Time) error {
	switch expVal := exp.(type) {
	case time.Time:
		if now.After(expVal) {
			return fmt.Errorf("token has expired")
		}
	case float64:
		expTime := time.Unix(int64(expVal), 0)
		if now.After(expTime) {
			return fmt.Errorf("token has expired")
		}
	case json.Number:
		if expInt, err := expVal.Int64(); err == nil {
			expTime := time.Unix(expInt, 0)
			if now.After(expTime) {
				return fmt.Errorf("token has expired")
			}
		} else {
			return fmt.Errorf("invalid exp value: %v", expVal)
		}
	default:
		return fmt.Errorf("unsupported exp type: %T", expVal)
	}
	return nil
}

func (j *JWTAuthenticator) validateNotBefore(nbf interface{}, now time.Time) error {
	switch nbfVal := nbf.(type) {
	case time.Time:
		if now.Before(nbfVal) {
			return fmt.Errorf("token not yet valid")
		}
	case float64:
		nbfTime := time.Unix(int64(nbfVal), 0)
		if now.Before(nbfTime) {
			return fmt.Errorf("token not yet valid")
		}
	case json.Number:
		if nbfInt, err := nbfVal.Int64(); err == nil {
			nbfTime := time.Unix(nbfInt, 0)
			if now.Before(nbfTime) {
				return fmt.Errorf("token not yet valid")
			}
		} else {
			return fmt.Errorf("invalid nbf value: %v", nbfVal)
		}
	default:
		return fmt.Errorf("unsupported nbf type: %T", nbfVal)
	}
	return nil
}

func (j *JWTAuthenticator) validateIssuedAt(iat interface{}, now time.Time) error {
	switch iatVal := iat.(type) {
	case time.Time:
		// iat should be before or equal to the current time
		// Allow a 1-minute clock skew
		if now.Add(1 * time.Minute).Before(iatVal) {
			return fmt.Errorf("token issued in the future")
		}
	case float64:
		iatTime := time.Unix(int64(iatVal), 0)
		if now.Add(1 * time.Minute).Before(iatTime) {
			return fmt.Errorf("token issued in the future")
		}
	case json.Number:
		if iatInt, err := iatVal.Int64(); err == nil {
			iatTime := time.Unix(iatInt, 0)
			if now.Add(1 * time.Minute).Before(iatTime) {
				return fmt.Errorf("token issued in the future")
			}
		} else {
			return fmt.Errorf("invalid iat value: %v", iatVal)
		}
	default:
		return fmt.Errorf("unsupported iat type: %T", iatVal)
	}
	return nil
}

// ValidateToken validates a JWT token and sets user information in the context
func (j *JWTAuthenticator) ValidateToken(ctx context.Context, c *gin.Context, token string) error {
	if !j.enabled {
		return nil
	}

	if token == "" {
		return fmt.Errorf("authorization header missing or empty")
	}

	parsed, sub, err := j.parseAndValidate(token)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	c.Set(common.JWTTokenKey, parsed)
	c.Set(common.UserIdKey, sub)
	return nil
}

// IsEnabled returns whether JWT authentication is enabled
func (j *JWTAuthenticator) IsEnabled() bool {
	return j.enabled
}

// ClaimFromContext returns the named JWT claim from a validated token stored on the Gin context.
func ClaimFromContext(c *gin.Context, claimName string) string {
	if c == nil || claimName == "" {
		return ""
	}
	val, ok := c.Get(common.JWTTokenKey)
	if !ok {
		return ""
	}
	token, ok := val.(jwt.Token)
	if !ok || token == nil {
		return ""
	}
	return claimString(token, claimName)
}

func claimString(token jwt.Token, claimName string) string {
	var s string
	if err := token.Get(claimName, &s); err == nil && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	var raw any
	if err := token.Get(claimName, &raw); err != nil || raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

// Authenticate returns a Gin middleware for JWT token validation
func (j *JWTAuthenticator) Authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if j.enabled {
			// Extract and validate the JWT token
			token := extractTokenFromHeader(c.Request)
			if token == "" {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header missing or invalid"})
				return
			}

			parsed, sub, err := j.parseAndValidate(token)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("Unauthorized: %v", err)})
				return
			}
			c.Set(common.JWTTokenKey, parsed)
			c.Set(common.UserIdKey, sub)
		}
		c.Next()
	}
}
