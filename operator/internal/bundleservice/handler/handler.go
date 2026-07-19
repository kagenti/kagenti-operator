package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/rossoctl/operator/internal/bundleservice/identity"
	"github.com/rossoctl/operator/internal/bundleservice/provider"
)

type BundleProvider interface {
	GetBundle(ctx context.Context, id identity.ClientIdentity, ifNoneMatch string) ([]byte, string, error)
}

type ReadinessChecker interface {
	Ready() bool
}

type Handler struct {
	provider BundleProvider
	readyz   ReadinessChecker
	verifier identity.Verifier
}

func New(p BundleProvider, readyz ReadinessChecker, verifier identity.Verifier) *Handler {
	return &Handler{provider: p, readyz: readyz, verifier: verifier}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/bundles", h.serveBundle)
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/readyz", h.readyz_)
}

func (h *Handler) serveBundle(w http.ResponseWriter, r *http.Request) {
	slog.Info("bundle request received", "url", r.URL.String(), "method", r.Method)

	if h.readyz != nil && !h.readyz.Ready() {
		http.Error(w, "service not ready", http.StatusServiceUnavailable)
		return
	}

	id, err := identity.FromRequest(r)
	if err != nil {
		http.Error(w, "invalid client identity", http.StatusBadRequest)
		return
	}

	if err := h.verifier.Verify(r, id); err != nil {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	// Extract If-None-Match, strip quotes
	ifNoneMatch := r.Header.Get("If-None-Match")
	if len(ifNoneMatch) >= 2 && ifNoneMatch[0] == '"' && ifNoneMatch[len(ifNoneMatch)-1] == '"' {
		ifNoneMatch = ifNoneMatch[1 : len(ifNoneMatch)-1]
	}

	data, etag, err := h.provider.GetBundle(r.Context(), id, ifNoneMatch)
	if err != nil {
		if errors.Is(err, provider.ErrBundleTooLarge) {
			http.Error(w, "bundle too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// nil data with non-empty etag signals 304
	if data == nil && etag != "" {
		slog.Info("bundle response", "url", r.URL.String(), "status", http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	quotedETag := `"` + etag + `"`
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("ETag", quotedETag)
	w.Header().Set("Cache-Control", "max-age=0, must-revalidate")
	_, _ = w.Write(data)
	slog.Info("bundle response", "url", r.URL.String(), "status", http.StatusOK, "size", len(data))
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) readyz_(w http.ResponseWriter, _ *http.Request) {
	if h.readyz == nil || h.readyz.Ready() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}
