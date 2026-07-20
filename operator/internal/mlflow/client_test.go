/*
Copyright 2026.

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

package mlflow

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pathExperimentsCreate    = "/api/2.0/mlflow/experiments/create"
	pathExperimentsGetByName = "/api/2.0/mlflow/experiments/get-by-name"
)

func createTempTokenFile(t *testing.T) string {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	return tokenPath
}

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &Client{
		BaseURL:    server.URL,
		TokenPath:  createTempTokenFile(t),
		MaxRetries: -1,
	}
}

func writeConflictResponse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(mlflowError{
		ErrorCode: "RESOURCE_ALREADY_EXISTS",
		Message:   "Experiment already exists",
	})
}

func TestCreateExperiment_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != pathExperimentsCreate {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get(WorkspaceHeader) != "test-ns" {
			t.Errorf("unexpected workspace header: %s", r.Header.Get(WorkspaceHeader))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content-type: %s", r.Header.Get("Content-Type"))
		}

		var req createExperimentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Name != "my-experiment" {
			t.Errorf("unexpected experiment name: %s", req.Name)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(createExperimentResponse{ExperimentID: "42"})
	})

	c := newTestClient(t, handler)
	id, err := c.CreateExperiment(context.Background(), "my-experiment", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "42" {
		t.Errorf("expected experiment ID 42, got %s", id)
	}
}

func TestCreateExperiment_AlreadyExists_FallsBackToGetByName(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathExperimentsCreate:
			writeConflictResponse(w)
		case pathExperimentsGetByName:
			if r.URL.Query().Get("experiment_name") != "existing-exp" {
				t.Errorf("unexpected experiment name query: %s", r.URL.Query().Get("experiment_name"))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"experiment": map[string]interface{}{
					"experiment_id":   "99",
					"lifecycle_stage": "active",
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	c := newTestClient(t, handler)
	id, err := c.CreateExperiment(context.Background(), "existing-exp", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "99" {
		t.Errorf("expected experiment ID 99, got %s", id)
	}
}

func TestCreateExperiment_AlreadyExists_Deleted_RestoresAndReturns(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathExperimentsCreate:
			writeConflictResponse(w)
		case pathExperimentsGetByName:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"experiment": map[string]interface{}{
					"experiment_id":   "77",
					"lifecycle_stage": "deleted",
				},
			})
		case "/api/2.0/mlflow/experiments/restore":
			var req restoreExperimentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("failed to decode restore request: %v", err)
			}
			if req.ExperimentID != "77" {
				t.Errorf("unexpected experiment ID in restore: %s", req.ExperimentID)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	c := newTestClient(t, handler)
	id, err := c.CreateExperiment(context.Background(), "deleted-exp", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "77" {
		t.Errorf("expected experiment ID 77, got %s", id)
	}
}

func TestCreateExperiment_APIError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(mlflowError{
			ErrorCode: "INTERNAL_ERROR",
			Message:   "Something went wrong",
		})
	})

	c := newTestClient(t, handler)
	_, err := c.CreateExperiment(context.Background(), "fail-exp", "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	mlErr, ok := err.(*mlflowError)
	if !ok {
		t.Fatalf("expected *mlflowError, got %T: %v", err, err)
	}
	if mlErr.ErrorCode != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR, got %s", mlErr.ErrorCode)
	}
}

func TestCreateExperiment_NonJSONError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})

	c := newTestClient(t, handler)
	_, err := c.CreateExperiment(context.Background(), "fail-exp", "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetExperimentByName_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathExperimentsGetByName {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("experiment_name") != "test-exp" {
			t.Errorf("unexpected experiment name: %s", r.URL.Query().Get("experiment_name"))
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"experiment": map[string]interface{}{
				"experiment_id":   "55",
				"lifecycle_stage": "active",
			},
		})
	})

	c := newTestClient(t, handler)
	id, err := c.GetExperimentByName(context.Background(), "test-exp", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "55" {
		t.Errorf("expected experiment ID 55, got %s", id)
	}
}

func TestGetExperimentByName_NotFound(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(mlflowError{
			ErrorCode: "RESOURCE_DOES_NOT_EXIST",
			Message:   "No experiment found",
		})
	})

	c := newTestClient(t, handler)
	_, err := c.GetExperimentByName(context.Background(), "missing-exp", "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClient_TokenFileNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	c := &Client{
		BaseURL:    server.URL,
		TokenPath:  "/nonexistent/path/token",
		MaxRetries: -1,
	}

	_, err := c.CreateExperiment(context.Background(), "test", "ns")
	if err == nil {
		t.Fatal("expected error when token file is missing")
	}
}

func TestClient_DefaultTokenPath(t *testing.T) {
	c := &Client{BaseURL: "http://example.com"}
	if c.tokenPath() != DefaultTokenPath {
		t.Errorf("expected default token path %s, got %s", DefaultTokenPath, c.tokenPath())
	}
}

func TestClient_CustomTokenPath(t *testing.T) {
	c := &Client{BaseURL: "http://example.com", TokenPath: "/custom/token"}
	if c.tokenPath() != "/custom/token" {
		t.Errorf("expected /custom/token, got %s", c.tokenPath())
	}
}

func TestIsResourceAlreadyExists(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "matching error",
			err:      &mlflowError{ErrorCode: "RESOURCE_ALREADY_EXISTS", Message: "exists"},
			expected: true,
		},
		{
			name:     "different error code",
			err:      &mlflowError{ErrorCode: "INTERNAL_ERROR", Message: "fail"},
			expected: false,
		},
		{
			name:     "wrapped matching error",
			err:      fmt.Errorf("outer: %w", &mlflowError{ErrorCode: "RESOURCE_ALREADY_EXISTS", Message: "exists"}),
			expected: true,
		},
		{
			name:     "non-mlflow error",
			err:      context.DeadlineExceeded,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsResourceAlreadyExists(tt.err); got != tt.expected {
				t.Errorf("IsResourceAlreadyExists() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestMlflowError_Error(t *testing.T) {
	e := &mlflowError{ErrorCode: "TEST_CODE", Message: "test message"}
	expected := "mlflow: TEST_CODE: test message"
	if e.Error() != expected {
		t.Errorf("expected %q, got %q", expected, e.Error())
	}
}

func TestRestoreExperiment_Failure(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathExperimentsCreate:
			writeConflictResponse(w)
		case pathExperimentsGetByName:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"experiment": map[string]interface{}{
					"experiment_id":   "88",
					"lifecycle_stage": "deleted",
				},
			})
		case "/api/2.0/mlflow/experiments/restore":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("restore failed"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	c := newTestClient(t, handler)
	_, err := c.CreateExperiment(context.Background(), "restore-fail", "test-ns")
	if err == nil {
		t.Fatal("expected error on restore failure")
	}
}

// --- Retry / timeout tests ---

func TestRetry_SucceedsAfterTransientFailure(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(createExperimentResponse{ExperimentID: "42"})
	})

	c := newTestClient(t, handler)
	c.MaxRetries = 3
	c.RetryBaseDelay = 1 * time.Millisecond

	id, err := c.CreateExperiment(context.Background(), "retry-exp", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "42" {
		t.Errorf("expected experiment ID 42, got %s", id)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 requests, got %d", got)
	}
}

func TestRetry_ExhaustsRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(mlflowError{
			ErrorCode: "INTERNAL_ERROR",
			Message:   "server error",
		})
	})

	c := newTestClient(t, handler)
	c.MaxRetries = 2
	c.RetryBaseDelay = 1 * time.Millisecond

	_, err := c.CreateExperiment(context.Background(), "exhaust-exp", "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// 1 initial + 2 retries = 3 total
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 requests, got %d", got)
	}
}

func TestRetry_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	})

	c := newTestClient(t, handler)
	c.MaxRetries = 3
	c.RetryBaseDelay = 1 * time.Millisecond

	_, err := c.CreateExperiment(context.Background(), "no-retry-exp", "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 request (no retries on 4xx), got %d", got)
	}
}

func TestBuildTransport_NoCAFile(t *testing.T) {
	c := &Client{BaseURL: "https://example.com"}
	transport, err := c.buildTransport()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transport != nil {
		t.Error("expected nil transport when CAFile is empty")
	}
}

func TestBuildTransport_InvalidCAFile(t *testing.T) {
	c := &Client{BaseURL: "https://example.com", CAFile: "/nonexistent/ca.pem"}
	_, err := c.buildTransport()
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestBuildTransport_InvalidPEM(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(caPath, []byte("not a pem"), 0600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	c := &Client{BaseURL: "https://example.com", CAFile: caPath}
	_, err := c.buildTransport()
	if err == nil {
		t.Fatal("expected error for invalid PEM content")
	}
}

func TestBuildTransport_ValidCAFile(t *testing.T) {
	// Generate a self-signed CA certificate for testing
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := generateTestCACert(t)
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatalf("writing test CA: %v", err)
	}

	c := &Client{BaseURL: "https://example.com", CAFile: caPath}
	transport, err := c.buildTransport()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transport == nil {
		t.Fatal("expected non-nil transport with valid CA file")
	}
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if httpTransport.TLSClientConfig == nil || httpTransport.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected TLS config with custom RootCAs")
	}
}

func generateTestCACert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}
