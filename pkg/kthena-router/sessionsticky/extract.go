/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package sessionsticky

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/auth"
)

// ExtractSessionKey returns the first non-empty session key from spec.Sources.
func ExtractSessionKey(c *gin.Context, spec *networkingv1alpha1.SessionSticky, authenticator *auth.JWTAuthenticator) string {
	if spec == nil || len(spec.Sources) == 0 {
		return ""
	}
	for i := range spec.Sources {
		src := &spec.Sources[i]
		if v := extractOne(c.Request, src, authenticator); v != "" {
			return v
		}
	}
	return ""
}

func extractOne(req *http.Request, src *networkingv1alpha1.SessionKeySource, authenticator *auth.JWTAuthenticator) string {
	if src == nil || src.Name == "" {
		return ""
	}
	switch src.Type {
	case networkingv1alpha1.SessionKeySourceHeader:
		return strings.TrimSpace(req.Header.Get(src.Name))
	case networkingv1alpha1.SessionKeySourceQuery:
		return strings.TrimSpace(req.URL.Query().Get(src.Name))
	case networkingv1alpha1.SessionKeySourceCookie:
		for _, ck := range req.Cookies() {
			if ck.Name == src.Name {
				return strings.TrimSpace(ck.Value)
			}
		}
		return ""
	case networkingv1alpha1.SessionKeySourceJWTClaim:
		if authenticator == nil {
			return ""
		}
		return strings.TrimSpace(authenticator.StringClaimFromRequest(req, src.Name))
	default:
		return ""
	}
}
