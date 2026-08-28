package controller

import (
	"context"
	"testing"
	"time"
)

func TestFetchJWTSVID_InvalidSocketPath(t *testing.T) {
	// Test that fetchJWTSVID returns an error when the socket path is invalid
	r := &ClientRegistrationReconciler{
		SpiffeSocket: "unix:///nonexistent/socket.sock",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := r.fetchJWTSVID(ctx, "test-audience")
	if err == nil {
		t.Fatal("expected error when connecting to nonexistent socket, got nil")
	}

	// Error should mention client creation failure
	errMsg := err.Error()
	if errMsg == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestFetchJWTSVID_EmptySocketPath(t *testing.T) {
	// Test that fetchJWTSVID handles empty socket path gracefully
	r := &ClientRegistrationReconciler{
		SpiffeSocket: "",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := r.fetchJWTSVID(ctx, "test-audience")
	if err == nil {
		t.Fatal("expected error when socket path is empty, got nil")
	}
}

// NOTE: Full integration tests with a real SPIRE agent require:
// 1. Running SPIRE server and agent
// 2. Properly configured workload attestation
// 3. Valid SPIFFE trust domain
//
// These are better suited for E2E tests (e.g., operator/test/e2e/) rather than
// unit tests. The tests above verify error handling for the common failure cases.
//
// For E2E token exchange verification, see:
// - rossoctl/tests/e2e/ (main repo E2E tests)
// - .github/scripts/operator/ (deployment scripts that test token exchange)
