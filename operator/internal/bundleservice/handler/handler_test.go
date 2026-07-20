package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/operator/internal/bundleservice/identity"
	"github.com/rossoctl/operator/internal/bundleservice/provider"
)

type mockProvider struct {
	bundles map[string]struct {
		data []byte
		etag string
	}
}

func (m *mockProvider) GetBundle(_ context.Context, id identity.ClientIdentity, ifNoneMatch string) ([]byte, string, error) {
	key := id.CacheKey()
	b, ok := m.bundles[key]
	if !ok {
		return []byte("empty-bundle"), "sha256:empty", nil
	}
	if ifNoneMatch != "" && ifNoneMatch == b.etag {
		return nil, b.etag, nil
	}
	return b.data, b.etag, nil
}

type mockReadiness struct {
	ready bool
}

func (m *mockReadiness) Ready() bool { return m.ready }

func newTestHandler(p BundleProvider, readyz ReadinessChecker) http.Handler {
	h := New(p, readyz, identity.NoopVerifier{})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func bundleURL(ns, name string) string {
	return "/bundles?spiffe=localtest.me/ns/" + ns + "/sa/" + name
}

func TestServeBundle_200(t *testing.T) {
	p := &mockProvider{bundles: map[string]struct {
		data []byte
		etag string
	}{
		"default/my-agent": {data: []byte("bundle-data"), etag: "sha256:abc123"},
	}}

	srv := newTestHandler(p, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", bundleURL("default", "my-agent"), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("unexpected content-type: %s", w.Header().Get("Content-Type"))
	}
	if w.Header().Get("ETag") != `"sha256:abc123"` {
		t.Fatalf("unexpected etag: %s", w.Header().Get("ETag"))
	}
	if w.Body.String() != "bundle-data" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestServeBundle_304(t *testing.T) {
	p := &mockProvider{bundles: map[string]struct {
		data []byte
		etag string
	}{
		"default/my-agent": {data: []byte("bundle-data"), etag: "sha256:abc123"},
	}}

	srv := newTestHandler(p, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", bundleURL("default", "my-agent"), nil)
	req.Header.Set("If-None-Match", `"sha256:abc123"`)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", w.Code)
	}
}

func TestServeBundle_UnknownClient(t *testing.T) {
	p := &mockProvider{bundles: map[string]struct {
		data []byte
		etag string
	}{}}

	srv := newTestHandler(p, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", bundleURL("default", "no-such-agent"), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServeBundle_BundleTooLarge(t *testing.T) {
	p := &errorProvider{err: provider.ErrBundleTooLarge}

	srv := newTestHandler(p, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", bundleURL("default", "big-agent"), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestServeBundle_MissingQueryParam(t *testing.T) {
	p := &mockProvider{}
	srv := newTestHandler(p, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", "/bundles", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestServeBundle_InvalidSpiffeID(t *testing.T) {
	p := &mockProvider{}
	srv := newTestHandler(p, &mockReadiness{ready: true})

	cases := []struct {
		name string
		url  string
	}{
		{"missing path", "/bundles?spiffe=localtest.me"},
		{"wrong format", "/bundles?spiffe=localtest.me/wrong/path"},
		{"empty value", "/bundles?spiffe="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.url, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s, got %d: %s", tc.url, w.Code, w.Body.String())
			}
		})
	}
}

func TestServeBundle_NotReady(t *testing.T) {
	p := &mockProvider{}
	srv := newTestHandler(p, &mockReadiness{ready: false})
	req := httptest.NewRequest("GET", bundleURL("default", "my-agent"), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestHandler(&mockProvider{}, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestReadyz_Ready(t *testing.T) {
	srv := newTestHandler(&mockProvider{}, &mockReadiness{ready: true})
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestReadyz_NotReady(t *testing.T) {
	srv := newTestHandler(&mockProvider{}, &mockReadiness{ready: false})
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

type errorProvider struct {
	err error
}

func (e *errorProvider) GetBundle(_ context.Context, _ identity.ClientIdentity, _ string) ([]byte, string, error) {
	return nil, "", e.err
}
