package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/operator/internal/keycloak/testidp"
)

// TestAuthFlow_TokenAcquisition validates the full operator-to-workload auth flow:
// admin registers a client via the Keycloak admin API, then a workload uses the
// resulting credentials to obtain an access token via client_credentials grant.
func TestAuthFlow_TokenAcquisition(t *testing.T) {
	idp := testidp.Start(t)

	a := Admin{BaseURL: idp.URL(), HTTPClient: idp.Client()}
	clientID := "test-ns/my-workload"

	internalID, secret, err := a.RegisterOrFetchClient(context.Background(), "admin", "admin", ClientRegistrationParams{
		Realm:      "rossoctl",
		ClientID:   clientID,
		ClientName: clientID,
	})
	if err != nil {
		t.Fatalf("RegisterOrFetchClient: %v", err)
	}
	if internalID == "" {
		t.Fatal("expected non-empty internal ID")
	}

	token, err := idp.RequestWorkloadToken(clientID, secret)
	if err != nil {
		t.Fatalf("workload token request: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty access token")
	}

	// Verify the token is recognized as valid by the IdP
	if _, valid := idp.ValidateToken(token); !valid {
		t.Fatal("token should be valid immediately after issuance")
	}
}

// TestAuthFlow_TokenAcquisition_InvalidCredentials ensures the fake IdP rejects
// bad client credentials the same way a real Keycloak would.
func TestAuthFlow_TokenAcquisition_InvalidCredentials(t *testing.T) {
	idp := testidp.Start(t)
	idp.RegisterClient("my-client", "correct-secret")

	_, err := idp.RequestWorkloadToken("my-client", "wrong-secret")
	if err == nil {
		t.Fatal("expected error for invalid credentials")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 in error, got: %v", err)
	}
}

// TestAuthFlow_PolicyEnforcement validates that a proxy enforcing bearer token auth
// (modeled after Envoy's ext_authz pattern) correctly allows/rejects requests.
func TestAuthFlow_PolicyEnforcement(t *testing.T) {
	idp := testidp.Start(t)
	idp.RegisterClient("test-ns/agent", "agent-secret")

	token, err := idp.RequestWorkloadToken("test-ns/agent", "agent-secret")
	if err != nil {
		t.Fatalf("token request: %v", err)
	}

	// Build a minimal "enforcing proxy" that validates tokens via introspection
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		bearerToken := strings.TrimPrefix(authHeader, "Bearer ")
		if bearerToken == authHeader {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Introspect via the IdP
		form := url.Values{"token": {bearerToken}}
		resp, err := idp.Client().Post(
			idp.IntrospectionEndpoint(),
			"application/x-www-form-urlencoded",
			strings.NewReader(form.Encode()),
		)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(resp.Body)

		var result struct {
			Active bool `json:"active"`
		}
		if err := json.Unmarshal(body, &result); err != nil || !result.Active {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer proxy.Close()

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "valid token returns 200",
			authHeader: "Bearer " + token,
			wantStatus: http.StatusOK,
		},
		{
			name:       "no auth header returns 401",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid token returns 403",
			authHeader: "Bearer invalid-token-value",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "malformed auth header returns 401",
			authHeader: "Basic dXNlcjpwYXNz",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/resource", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// TestAuthFlow_TokenRefreshExpiry validates behavior when tokens expire:
// obtain a token, verify it works, wait for expiry, verify rejection,
// then re-acquire and verify the new token works.
func TestAuthFlow_TokenRefreshExpiry(t *testing.T) {
	idp := testidp.Start(t, testidp.WithTokenTTL(50*time.Millisecond))
	idp.RegisterClient("test-ns/ephemeral", "eph-secret")

	// Obtain initial token
	token1, err := idp.RequestWorkloadToken("test-ns/ephemeral", "eph-secret")
	if err != nil {
		t.Fatalf("first token request: %v", err)
	}
	if _, valid := idp.ValidateToken(token1); !valid {
		t.Fatal("token1 should be valid immediately")
	}

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)

	if _, valid := idp.ValidateToken(token1); valid {
		t.Fatal("token1 should be expired after TTL")
	}

	// Acquire a fresh token (simulates Auth Bridge refresh)
	token2, err := idp.RequestWorkloadToken("test-ns/ephemeral", "eph-secret")
	if err != nil {
		t.Fatalf("second token request: %v", err)
	}
	if token2 == token1 {
		t.Fatal("refreshed token should be different from expired token")
	}
	if _, valid := idp.ValidateToken(token2); !valid {
		t.Fatal("token2 should be valid")
	}
}

// TestAuthFlow_EndToEnd_RegisterAndAuthenticate exercises the full operator flow:
// admin token -> register client -> audience scope -> workload authenticates.
func TestAuthFlow_EndToEnd_RegisterAndAuthenticate(t *testing.T) {
	idp := testidp.Start(t)

	a := Admin{BaseURL: idp.URL(), HTTPClient: idp.Client()}
	ctx := context.Background()

	// Step 1: Admin obtains token
	adminToken, err := a.PasswordGrantToken(ctx, "admin", "admin")
	if err != nil {
		t.Fatalf("admin token: %v", err)
	}
	if adminToken == "" {
		t.Fatal("expected non-empty admin token")
	}

	// Step 2: Register client with token
	clientID := "production/my-agent"
	internalID, secret, err := a.RegisterOrFetchClientWithToken(ctx, adminToken, ClientRegistrationParams{
		Realm:               "rossoctl",
		ClientID:            clientID,
		ClientName:          clientID,
		TokenExchangeEnable: true,
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	if internalID == "" || secret == "" {
		t.Fatalf("expected non-empty id=%q secret=%q", internalID, secret)
	}

	// Step 3: Ensure audience scope (best-effort, like the operator)
	err = a.EnsureAudienceScope(ctx, adminToken, AudienceParams{
		Realm:                "rossoctl",
		ClientName:           clientID,
		AudienceClientID:     clientID,
		PlatformClientIDs:    []string{"rossoctl"},
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatalf("ensure audience scope: %v", err)
	}

	// Step 4: Workload authenticates using registered credentials
	workloadToken, err := idp.RequestWorkloadToken(clientID, secret)
	if err != nil {
		t.Fatalf("workload token: %v", err)
	}
	if workloadToken == "" {
		t.Fatal("expected non-empty workload token")
	}
	if cid, valid := idp.ValidateToken(workloadToken); !valid || cid != clientID {
		t.Fatalf("token validation: valid=%v clientID=%q", valid, cid)
	}
}
