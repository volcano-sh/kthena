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

// LookupBinding extracts the session key and returns a mapped ModelServer+Pod binding when present.
func LookupBinding(c *gin.Context, route types.NamespacedName, spec *networkingv1alpha1.SessionSticky, store Store) (sessionKey, storeKey string, binding Binding, ok bool) {
	if spec == nil || store == nil || c == nil {
		return "", "", Binding{}, false
	}
	sessionKey = ExtractSessionKey(c, spec)
	if sessionKey == "" {
		return "", "", Binding{}, false
	}
	storeKey = MappingKey(route, sessionKey)
	binding, ok = store.Get(c.Request.Context(), storeKey)
	if !ok || !binding.Valid() {
		return sessionKey, storeKey, Binding{}, false
	}
	return sessionKey, storeKey, binding, true
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
