/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package sessionsticky

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestExtractSessionKey_HeaderFirstWins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/foo?sid=q1", nil)
	req.Header.Set("X-A", "a1")
	req.Header.Set("X-B", "b1")
	c.Request = req

	spec := &networkingv1alpha1.SessionSticky{
		Sources: []networkingv1alpha1.SessionKeySource{
			{Type: networkingv1alpha1.SessionKeySourceHeader, Name: "X-A"},
			{Type: networkingv1alpha1.SessionKeySourceQuery, Name: "sid"},
		},
	}
	got := ExtractSessionKey(c, spec)
	require.Equal(t, "a1", got)
}

func TestExtractSessionKey_QueryFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/foo?sid=qval", nil)
	c.Request = req

	spec := &networkingv1alpha1.SessionSticky{
		Sources: []networkingv1alpha1.SessionKeySource{
			{Type: networkingv1alpha1.SessionKeySourceHeader, Name: "Missing"},
			{Type: networkingv1alpha1.SessionKeySourceQuery, Name: "sid"},
		},
	}
	require.Equal(t, "qval", ExtractSessionKey(c, spec))
}

func TestExtractSessionKey_Cookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "cval"})
	c.Request = req

	spec := &networkingv1alpha1.SessionSticky{
		Sources: []networkingv1alpha1.SessionKeySource{
			{Type: networkingv1alpha1.SessionKeySourceCookie, Name: "sid"},
		},
	}
	require.Equal(t, "cval", ExtractSessionKey(c, spec))
}

func TestExtractSessionKey_NilSpec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	require.Equal(t, "", ExtractSessionKey(c, nil))
}

func TestMemoryStore_GetSetTTL(t *testing.T) {
	ctx := t.Context()
	s := NewMemoryStore()
	key := "k1"
	_, ok := s.Get(ctx, key)
	require.False(t, ok)

	_, err := s.Commit(ctx, key, "pod-a", 2*time.Second)
	require.NoError(t, err)
	v, ok := s.Get(ctx, key)
	require.True(t, ok)
	require.Equal(t, "pod-a", v)

	out, err := s.Commit(ctx, key, "pod-b", 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "pod-a", out)
}

func TestMemoryStore_GetMissAfterTTL(t *testing.T) {
	ctx := t.Context()
	s := NewMemoryStore()
	key := "k-ttl"
	_, err := s.Commit(ctx, key, "pod-a", 50*time.Millisecond)
	require.NoError(t, err)

	time.Sleep(80 * time.Millisecond)
	_, ok := s.Get(ctx, key)
	require.False(t, ok)
	_, ok = s.Get(ctx, key)
	require.False(t, ok)
}

func TestMappingKey_Deterministic(t *testing.T) {
	a := MappingKey(types.NamespacedName{Namespace: "ns", Name: "r"}, "sess")
	b := MappingKey(types.NamespacedName{Namespace: "ns", Name: "r"}, "sess")
	require.Equal(t, a, b)
	require.NotEqual(t, a, MappingKey(types.NamespacedName{Namespace: "ns", Name: "r2"}, "sess"))
}
