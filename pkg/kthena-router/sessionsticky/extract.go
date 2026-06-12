/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package sessionsticky

import (
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/types"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/auth"
)

// ExtractSessionKey returns the first non-empty session key from spec.Sources.
func ExtractSessionKey(c *gin.Context, spec *networkingv1alpha1.SessionSticky) string {
	if spec == nil || len(spec.Sources) == 0 {
		return ""
	}
	for i := range spec.Sources {
		src := &spec.Sources[i]
		if v := extractOne(c, src); v != "" {
			return v
		}
	}
	return ""
}

// LookupHint extracts the session key and returns a mapped Pod name from the store when present.
func LookupHint(c *gin.Context, route types.NamespacedName, spec *networkingv1alpha1.SessionSticky, store Store) (sessionKey, storeKey, hint string) {
	if spec == nil || store == nil || c == nil {
		return "", "", ""
	}
	sessionKey = ExtractSessionKey(c, spec)
	if sessionKey == "" {
		return "", "", ""
	}
	storeKey = MappingKey(route, sessionKey)
	if mapped, ok := store.Get(c.Request.Context(), storeKey); ok && mapped != "" {
		hint = mapped
	}
	return sessionKey, storeKey, hint
}

func extractOne(c *gin.Context, src *networkingv1alpha1.SessionKeySource) string {
	req := c.Request
	if src == nil || src.Name == "" {
		return ""
	}
	switch src.Type {
	case networkingv1alpha1.SessionKeySourceHeader:
		return strings.TrimSpace(req.Header.Get(src.Name))
	case networkingv1alpha1.SessionKeySourceQuery:
		return strings.TrimSpace(req.URL.Query().Get(src.Name))
	case networkingv1alpha1.SessionKeySourceCookie:
		if ck, err := req.Cookie(src.Name); err == nil {
			return strings.TrimSpace(ck.Value)
		}
		return ""
	case networkingv1alpha1.SessionKeySourceJWTClaim:
		return strings.TrimSpace(auth.ClaimFromContext(c, src.Name))
	default:
		return ""
	}
}
