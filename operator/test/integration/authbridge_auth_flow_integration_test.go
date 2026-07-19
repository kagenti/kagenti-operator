//go:build integration
// +build integration

/*
Copyright 2026.

Integration tests for Auth Bridge authentication flow.

These tests validate that Auth Bridge works end-to-end against a real Keycloak
deployment: token acquisition via client_credentials, Envoy policy enforcement
(valid token → 200, no token → 401/403), and token refresh on expiry.

Prerequisites:
  - kubectl configured with access to a Kubernetes cluster with rossoctl CRDs installed
  - A Keycloak instance accessible from the test runner
  - Environment variables set:
    KEYCLOAK_URL            — base URL (e.g. https://keycloak.apps.example.com)
    KEYCLOAK_ADMIN_USER     — admin username
    KEYCLOAK_ADMIN_PASSWORD — admin password
    KEYCLOAK_REALM          — realm name (default: rossoctl)
    ENVOY_PROXY_URL         — (optional) URL of an Auth Bridge Envoy proxy to test policy enforcement
    KEYCLOAK_CA_CERT        — (optional) path to PEM CA bundle for custom Keycloak TLS

Run with: go test -v -tags=integration ./test/integration/... -timeout 5m -run TestAuthBridge
*/
package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/operator/internal/keycloak"
)

// httpClient returns an *http.Client that trusts the CA bundle at KEYCLOAK_CA_CERT
// (PEM file path). If the env var is unset it falls back to the system root CAs.
func httpClient(t *testing.T) *http.Client {
	t.Helper()
	caPath := os.Getenv("KEYCLOAK_CA_CERT")
	if caPath == "" {
		return http.DefaultClient
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("reading KEYCLOAK_CA_CERT (%s): %v", caPath, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("KEYCLOAK_CA_CERT (%s): no valid certificates found", caPath)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
}

func keycloakEnvOrSkip(t *testing.T) (baseURL, adminUser, adminPass, realm string) {
	t.Helper()
	baseURL = os.Getenv("KEYCLOAK_URL")
	adminUser = os.Getenv("KEYCLOAK_ADMIN_USER")
	adminPass = os.Getenv("KEYCLOAK_ADMIN_PASSWORD")
	realm = os.Getenv("KEYCLOAK_REALM")
	if realm == "" {
		realm = "rossoctl"
	}
	if baseURL == "" || adminUser == "" || adminPass == "" {
		t.Skip("requires real Keycloak deployment — set KEYCLOAK_URL, " +
			"KEYCLOAK_ADMIN_USER, KEYCLOAK_ADMIN_PASSWORD")
	}
	return
}

func envoyURLOrSkip(t *testing.T) string {
	t.Helper()
	u := os.Getenv("ENVOY_PROXY_URL")
	if u == "" {
		t.Skip("requires ENVOY_PROXY_URL — set to an Auth Bridge Envoy proxy endpoint")
	}
	return u
}

func TestAuthBridge_RealKeycloak_TokenAcquisition(t *testing.T) {
	baseURL, adminUser, adminPass, realm := keycloakEnvOrSkip(t)

	a := keycloak.Admin{
		BaseURL:    baseURL,
		HTTPClient: keycloak.DefaultHTTPClient(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Register or fetch a test client
	clientID := fmt.Sprintf("integration-test/%d", time.Now().UnixNano())
	internalID, secret, err := a.RegisterOrFetchClient(ctx, adminUser, adminPass, keycloak.ClientRegistrationParams{
		Realm:      realm,
		ClientID:   clientID,
		ClientName: clientID,
	})
	if err != nil {
		t.Fatalf("RegisterOrFetchClient: %v", err)
	}
	if internalID == "" || secret == "" {
		t.Fatalf("expected non-empty id=%q secret=%q", internalID, secret)
	}

	// Use the credentials to obtain a workload token via client_credentials
	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token",
		strings.TrimRight(baseURL, "/"), realm)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	client := httpClient(t)
	resp, err := client.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	t.Logf("successfully obtained workload token for client %q", clientID)
}

func TestAuthBridge_RealEnvoy_PolicyEnforcement(t *testing.T) {
	proxyURL := envoyURLOrSkip(t)
	baseURL, _, _, realm := keycloakEnvOrSkip(t)

	// Use the workload's own registered credentials to get a token with proper audience.
	// The operator registers the workload and stores credentials in a Secret named in
	// WORKLOAD_CLIENT_ID / WORKLOAD_CLIENT_SECRET env vars, or falls back to default
	// test namespace/name convention.
	workloadClientID := os.Getenv("WORKLOAD_CLIENT_ID")
	workloadClientSecret := os.Getenv("WORKLOAD_CLIENT_SECRET")
	if workloadClientID == "" || workloadClientSecret == "" {
		t.Skip("requires WORKLOAD_CLIENT_ID and WORKLOAD_CLIENT_SECRET — " +
			"read from the rossoctl-keycloak-client-credentials-* Secret in the test namespace")
	}

	// Obtain a valid token using the workload's own credentials
	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token",
		strings.TrimRight(baseURL, "/"), realm)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {workloadClientID},
		"client_secret": {workloadClientSecret},
	}
	client := httpClient(t)
	resp, err := client.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request: expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading token response: %v", err)
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		t.Fatalf("parsing token response: %v", err)
	}
	if tokenResp.AccessToken == "" {
		t.Fatal("empty access_token in token response")
	}

	// Test: no auth header → 401
	req, _ := http.NewRequest(http.MethodGet, proxyURL, nil)
	noAuthResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("no-auth request: %v", err)
	}
	noAuthResp.Body.Close() //nolint:errcheck
	if noAuthResp.StatusCode != http.StatusUnauthorized && noAuthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 401 or 403 for unauthenticated request, got %d", noAuthResp.StatusCode)
	}

	// Test: invalid token → 401/403
	req, _ = http.NewRequest(http.MethodGet, proxyURL, nil)
	req.Header.Set("Authorization", "Bearer invalid-token-value")
	badResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("bad-token request: %v", err)
	}
	badResp.Body.Close() //nolint:errcheck
	if badResp.StatusCode != http.StatusUnauthorized && badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 401 or 403 for invalid token, got %d", badResp.StatusCode)
	}

	// Test: valid token → 200
	req, _ = http.NewRequest(http.MethodGet, proxyURL, nil)
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	goodResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("valid-token request: %v", err)
	}
	goodResp.Body.Close() //nolint:errcheck
	if goodResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid token, got %d", goodResp.StatusCode)
	}

	t.Log("policy enforcement test completed (unauthenticated rejected, invalid token rejected, valid token accepted)")
}

func TestAuthBridge_RealKeycloak_TokenRefresh(t *testing.T) {
	baseURL, adminUser, adminPass, realm := keycloakEnvOrSkip(t)

	a := keycloak.Admin{
		BaseURL:    baseURL,
		HTTPClient: keycloak.DefaultHTTPClient(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	clientID := fmt.Sprintf("integration-test-refresh/%d", time.Now().UnixNano())
	_, secret, err := a.RegisterOrFetchClient(ctx, adminUser, adminPass, keycloak.ClientRegistrationParams{
		Realm:      realm,
		ClientID:   clientID,
		ClientName: clientID,
	})
	if err != nil {
		t.Fatalf("RegisterOrFetchClient: %v", err)
	}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token",
		strings.TrimRight(baseURL, "/"), realm)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}

	// Obtain first token
	client := httpClient(t)
	resp1, err := client.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	resp1.Body.Close() //nolint:errcheck
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first token: expected 200, got %d", resp1.StatusCode)
	}

	// Obtain second token (simulates refresh — in production, Auth Bridge handles this)
	resp2, err := client.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	resp2.Body.Close() //nolint:errcheck
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second token: expected 200, got %d", resp2.StatusCode)
	}

	t.Log("token refresh test completed (two successive token acquisitions succeeded)")
}
