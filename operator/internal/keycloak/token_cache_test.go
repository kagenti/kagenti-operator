package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCachedAdminTokenProvider_Token(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testMasterRealmTokenPath {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "tok",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	a := &Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	var cache CachedAdminTokenProvider

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := cache.Token(ctx, a, "u", "p")
		if err != nil || tok != "tok" {
			t.Fatalf("iter %d: tok=%q err=%v", i, tok, err)
		}
	}
	if tokenCalls != 1 {
		t.Fatalf("expected 1 token HTTP call, got %d", tokenCalls)
	}
}

func TestCachedAdminTokenProvider_differentCredentials(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "tok",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	a := &Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	var cache CachedAdminTokenProvider
	ctx := context.Background()

	if _, err := cache.Token(ctx, a, "u1", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Token(ctx, a, "u2", "p"); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 2 {
		t.Fatalf("expected 2 token calls for different users, got %d", tokenCalls)
	}
}

func TestCachedAdminTokenProvider_refreshNearExpiry(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "tok",
			"expires_in":   90,
		})
	}))
	defer srv.Close()

	a := &Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	var cache CachedAdminTokenProvider
	ctx := context.Background()

	if _, err := cache.Token(ctx, a, "u", "p"); err != nil {
		t.Fatal(err)
	}
	// Force expiry inside skew window: cached expiresAt is now+90s; skew is 60s, so at now+31s we refresh.
	e := cache.entries[adminTokenCacheKey(srv.URL, "u")]
	e.expiresAt = time.Now().Add(30 * time.Second)
	cache.entries[adminTokenCacheKey(srv.URL, "u")] = e

	if _, err := cache.Token(ctx, a, "u", "p"); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 2 {
		t.Fatalf("expected refresh after near-expiry, tokenCalls=%d", tokenCalls)
	}
}
