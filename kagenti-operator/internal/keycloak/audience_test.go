package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAudienceScopeName(t *testing.T) {
	if got := AudienceScopeName("team1/my-agent"); got != "agent-team1-my-agent-aud" {
		t.Fatalf("got %q", got)
	}
}

func TestEnsureAudienceScope(t *testing.T) {
	var listScopesCalls, postScopeCalls, postMapperCalls, putRealmCalls, listKagentiCalls, putClientCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			listScopesCalls++
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{})
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodPost:
			postScopeCalls++
			w.Header().Set("Location", srv.URL+"/admin/realms/kagenti/client-scopes/new-scope-id")
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodPost:
			postMapperCalls++
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
				ID: "m1", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
				ProtocolMapper: "oidc-audience-mapper",
				Config:         map[string]string{"included.custom.audience": "ns/wl"},
			}})
		case path == "/admin/realms/kagenti/default-default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			putRealmCalls++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/admin/realms/kagenti/clients") && r.Method == http.MethodGet && r.URL.Query().Get("clientId") == "kagenti":
			listKagentiCalls++
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "plat-int", "clientId": "kagenti"}})
		case path == "/admin/realms/kagenti/clients/plat-int/default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			putClientCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     "ns/wl",
		PlatformClientIDs:    []string{"kagenti"},
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if listScopesCalls != 1 {
		t.Fatalf("listScopesCalls=%d", listScopesCalls)
	}
	if postScopeCalls != 1 || postMapperCalls != 1 || putRealmCalls != 1 || listKagentiCalls != 1 || putClientCalls != 1 {
		t.Fatalf("calls scope=%d mapper=%d realm=%d listK=%d putC=%d", postScopeCalls, postMapperCalls, putRealmCalls, listKagentiCalls, putClientCalls)
	}
}

// TestEnsureAudienceScope_UpdatesStaleMapper verifies that when an audience scope mapper
// already exists with a different audience (e.g. short-form "ns/wl" instead of SPIFFE URI),
// ensureAudienceMapper detects the mismatch and updates it via PUT.
func TestEnsureAudienceScope_UpdatesStaleMapper(t *testing.T) {
	var getMapperCalls, putMapperCalls int
	var putMapperBody protocolMapperRep
	spiffeURI := "spiffe://example.org/ns/ns/sa/wl"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		// Scope already exists
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{{ID: "scope-123", Name: "agent-ns-wl-aud"}})

		// POST mapper returns 409 (already exists)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)

		// GET mappers — first call returns stale, subsequent calls return corrected
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			getMapperCalls++
			aud := "ns/wl"
			if putMapperCalls > 0 {
				aud = spiffeURI
			}
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
				ID:             "mapper-456",
				Name:           "agent-ns-wl-aud",
				Protocol:       "openid-connect",
				ProtocolMapper: "oidc-audience-mapper",
				Config: map[string]string{
					"included.custom.audience": aud,
					"id.token.claim":           "false",
					"access.token.claim":       "true",
					"userinfo.token.claim":     "false",
				},
			}})

		// PUT mapper — update with correct audience
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models/mapper-456") && r.Method == http.MethodPut:
			putMapperCalls++
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putMapperBody)
			w.WriteHeader(http.StatusNoContent)

		// Realm default scope
		case path == "/admin/realms/kagenti/default-default-client-scopes/scope-123" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     spiffeURI,
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if getMapperCalls != 2 {
		t.Fatalf("expected 2 GET mapper calls (update + verify), got %d", getMapperCalls)
	}
	if putMapperCalls != 1 {
		t.Fatalf("expected 1 PUT mapper call, got %d", putMapperCalls)
	}
	if putMapperBody.Config["included.custom.audience"] != spiffeURI {
		t.Fatalf("expected audience %q, got %q", spiffeURI, putMapperBody.Config["included.custom.audience"])
	}
}

// TestEnsureAudienceScope_SkipsUpdateWhenCorrect verifies that when the existing mapper
// already has the correct audience, no PUT is issued.
func TestEnsureAudienceScope_SkipsUpdateWhenCorrect(t *testing.T) {
	var putMapperCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{{ID: "scope-123", Name: "agent-ns-wl-aud"}})

		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)

		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			spiffeURI := "spiffe://example.org/ns/ns/sa/wl"
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
				ID:             "mapper-456",
				Name:           "agent-ns-wl-aud",
				Protocol:       "openid-connect",
				ProtocolMapper: "oidc-audience-mapper",
				Config: map[string]string{
					"included.custom.audience": spiffeURI, // already correct
					"id.token.claim":           "false",
					"access.token.claim":       "true",
					"userinfo.token.claim":     "false",
				},
			}})

		case strings.Contains(path, "/protocol-mappers/models/mapper-456") && r.Method == http.MethodPut:
			putMapperCalls++
			w.WriteHeader(http.StatusNoContent)

		case path == "/admin/realms/kagenti/default-default-client-scopes/scope-123" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     "spiffe://example.org/ns/ns/sa/wl",
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if putMapperCalls != 0 {
		t.Fatalf("expected 0 PUT mapper calls (audience already correct), got %d", putMapperCalls)
	}
}

// TestEnsureAudienceScope_MapperFailurePropagated verifies that when the mapper POST
// returns a server error (e.g. 500), the error propagates to EnsureAudienceScope
// instead of being silently swallowed (regression test for #348).
func TestEnsureAudienceScope_MapperFailurePropagated(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		// Scope does not exist yet
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{})

		// Scope creation succeeds
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodPost:
			w.Header().Set("Location", srv.URL+"/admin/realms/kagenti/client-scopes/new-scope-id")
			w.WriteHeader(http.StatusCreated)

		// Mapper POST returns 500 (server error)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal"}`))

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     "spiffe://example.org/ns/ns/sa/wl",
		AudienceScopeEnabled: true,
	})
	if err == nil {
		t.Fatal("expected error when mapper POST fails, got nil")
	}
	if !strings.Contains(err.Error(), "ensure audience mapper") {
		t.Fatalf("expected error to contain 'ensure audience mapper', got: %s", err.Error())
	}
}

// TestEnsureAudienceScope_VerifyRecreatesMissingMapper verifies that the defense-in-depth
// verifyAudienceMapper check detects a scope that exists without a mapper (from a prior
// failed reconcile) and re-creates the mapper.
func TestEnsureAudienceScope_VerifyRecreatesMissingMapper(t *testing.T) {
	var verifyGetCalls, recreatePostCalls, verifyGetAfterRecreate int
	spiffeURI := "spiffe://example.org/ns/ns/sa/wl"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		// Scope already exists from prior run
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{{ID: "scope-123", Name: "agent-ns-wl-aud"}})

		// ensureAudienceMapper POST — mapper created (scope exists, mapper doesn't)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			recreatePostCalls++
			w.WriteHeader(http.StatusCreated)

		// GET mappers — first call (verify) returns empty (mapper missing), second returns recreated
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			verifyGetCalls++
			if recreatePostCalls > 0 {
				verifyGetAfterRecreate++
				_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
					ID: "m-new", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
					ProtocolMapper: "oidc-audience-mapper",
					Config:         map[string]string{"included.custom.audience": spiffeURI},
				}})
			} else {
				_ = json.NewEncoder(w).Encode([]protocolMapperRep{})
			}

		// Realm default scope
		case path == "/admin/realms/kagenti/default-default-client-scopes/scope-123" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     spiffeURI,
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifyGetCalls < 1 {
		t.Fatalf("expected at least 1 verify GET call, got %d", verifyGetCalls)
	}
	if recreatePostCalls < 1 {
		t.Fatalf("expected mapper to be re-created via POST, got %d calls", recreatePostCalls)
	}
}

// TestEnsureAudienceScope_DeletesWrongTypeMapper verifies that when a mapper with the
// correct name exists but has the wrong protocolMapper type (not oidc-audience-mapper),
// the operator deletes it and recreates the correct mapper. This is the fix for #358.
func TestEnsureAudienceScope_DeletesWrongTypeMapper(t *testing.T) {
	var deleteMapperCalls, recreatePostCalls int
	spiffeURI := "spiffe://example.org/ns/ns/sa/wl"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		// Scope already exists
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{{ID: "scope-123", Name: "agent-ns-wl-aud"}})

		// ensureAudienceMapper POST — 409 conflict (mapper name taken)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			if deleteMapperCalls > 0 {
				// After delete, the POST succeeds
				recreatePostCalls++
				w.WriteHeader(http.StatusCreated)
			} else {
				w.WriteHeader(http.StatusConflict)
			}

		// GET mappers — returns a mapper with wrong type (e.g. "oidc-hardcoded-claim-mapper")
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			if deleteMapperCalls > 0 {
				// After delete+recreate, verify sees the correct mapper
				_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
					ID: "m-new", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
					ProtocolMapper: "oidc-audience-mapper",
					Config:         map[string]string{"included.custom.audience": spiffeURI},
				}})
			} else {
				_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
					ID:             "mapper-stale",
					Name:           "agent-ns-wl-aud",
					Protocol:       "openid-connect",
					ProtocolMapper: "oidc-hardcoded-claim-mapper", // WRONG TYPE
					Config:         map[string]string{"claim.value": "something"},
				}})
			}

		// DELETE the stale mapper
		case strings.Contains(path, "/protocol-mappers/models/mapper-stale") && r.Method == http.MethodDelete:
			deleteMapperCalls++
			w.WriteHeader(http.StatusNoContent)

		// Realm default scope
		case path == "/admin/realms/kagenti/default-default-client-scopes/scope-123" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     spiffeURI,
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleteMapperCalls != 1 {
		t.Fatalf("expected 1 DELETE call for stale mapper, got %d", deleteMapperCalls)
	}
	if recreatePostCalls != 1 {
		t.Fatalf("expected 1 POST call to recreate mapper after delete, got %d", recreatePostCalls)
	}
}

// TestEnsureAudienceScope_PhantomConflict409 verifies that when the mapper POST
// returns 409 but the scope's mapper list is empty (phantom Keycloak conflict from
// concurrent reconcile or realm-level name collision), the function returns nil
// instead of entering an error loop. The verifyAudienceMapper defense-in-depth will
// retry on the next reconcile.
func TestEnsureAudienceScope_PhantomConflict409(t *testing.T) {
	var postCalls, getCalls int
	spiffeURI := "spiffe://example.org/ns/ns/sa/wl"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		// Scope already exists
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{{ID: "scope-123", Name: "agent-ns-wl-aud"}})

		// POST mapper always returns 409 (persistent realm-level collision)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			postCalls++
			w.WriteHeader(http.StatusConflict)

		// GET mappers — empty (the mapper doesn't actually exist in this scope)
		// Second GET (from verifyAudienceMapper) also empty — triggers ensureAudienceMapper
		// which hits 409 again and returns nil
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			getCalls++
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{})

		// Realm default scope
		case path == "/admin/realms/kagenti/default-default-client-scopes/scope-123" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     spiffeURI,
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatalf("expected nil error (phantom 409 should not loop), got: %v", err)
	}
	if postCalls < 1 {
		t.Fatalf("expected at least 1 POST attempt, got %d", postCalls)
	}
}

// TestEnsureAudienceScope_ConcurrentCreate409 verifies that when both the initial
// and retry POST return 409 (concurrent reconcile created the mapper), the operator
// treats it as success rather than entering an error loop.
func TestEnsureAudienceScope_ConcurrentCreate409(t *testing.T) {
	spiffeURI := "spiffe://example.org/ns/ns/sa/wl"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})

		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{{ID: "scope-123", Name: "agent-ns-wl-aud"}})

		// All POSTs return 409 (concurrent reconcile already created it)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)

		// GET mappers — empty during updateAudienceMapperIfNeeded (race: mapper not yet visible)
		// but present during verifyAudienceMapper (transaction committed)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
				ID: "m-concurrent", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
				ProtocolMapper: "oidc-audience-mapper",
				Config:         map[string]string{"included.custom.audience": spiffeURI},
			}})

		case path == "/admin/realms/kagenti/default-default-client-scopes/scope-123" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected %s %s", r.Method, path)
		}
	}))
	defer srv.Close()

	a := Admin{BaseURL: srv.URL, HTTPClient: srv.Client()}
	token, err := a.PasswordGrantToken(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}

	err = a.EnsureAudienceScope(context.Background(), token, AudienceParams{
		Realm:                "kagenti",
		ClientName:           "ns/wl",
		AudienceClientID:     spiffeURI,
		AudienceScopeEnabled: true,
	})
	if err != nil {
		t.Fatalf("expected success when concurrent reconcile created mapper, got: %v", err)
	}
}

func TestEnsureAudienceScope_Disabled(t *testing.T) {
	a := Admin{}
	err := a.EnsureAudienceScope(context.Background(), "t", AudienceParams{AudienceScopeEnabled: false})
	if err != nil {
		t.Fatal(err)
	}
}
