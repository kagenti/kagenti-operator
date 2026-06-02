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

// TestEnsureAudienceScope_AttachesToAgentClient verifies that when AgentClientUUID is set,
// the audience scope is explicitly attached to the agent's own client as a default-client-scope
// (in addition to the realm default-default registration). This is required because Keycloak
// does not retroactively attach realm default-defaults to clients that already exist.
func TestEnsureAudienceScope_AttachesToAgentClient(t *testing.T) {
	var putAgentClientCalls, putPlatformClientCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{})
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodPost:
			w.Header().Set("Location", srv.URL+"/admin/realms/kagenti/client-scopes/new-scope-id")
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
				ID: "m1", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
				ProtocolMapper: "oidc-audience-mapper",
				Config:         map[string]string{"included.custom.audience": "ns/wl"},
			}})
		case path == "/admin/realms/kagenti/default-default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/admin/realms/kagenti/clients") && r.Method == http.MethodGet && r.URL.Query().Get("clientId") == "kagenti":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "plat-int", "clientId": "kagenti"}})
		case path == "/admin/realms/kagenti/clients/agent-uuid-123/default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			putAgentClientCalls++
			w.WriteHeader(http.StatusNoContent)
		case path == "/admin/realms/kagenti/clients/plat-int/default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			putPlatformClientCalls++
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
		AgentClientUUID:      "agent-uuid-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if putAgentClientCalls != 1 {
		t.Fatalf("expected 1 PUT to agent client default-client-scopes, got %d", putAgentClientCalls)
	}
	if putPlatformClientCalls != 1 {
		t.Fatalf("expected 1 PUT to platform client default-client-scopes, got %d", putPlatformClientCalls)
	}
}

// TestEnsureAudienceScope_NoAgentAttachWhenUUIDEmpty verifies that omitting AgentClientUUID
// preserves the prior behavior — only platform clients receive explicit scope attachment.
func TestEnsureAudienceScope_NoAgentAttachWhenUUIDEmpty(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == testMasterRealmTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]clientScopeListItem{})
		case path == "/admin/realms/kagenti/client-scopes" && r.Method == http.MethodPost:
			w.Header().Set("Location", srv.URL+"/admin/realms/kagenti/client-scopes/new-scope-id")
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
				ID: "m1", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
				ProtocolMapper: "oidc-audience-mapper",
				Config:         map[string]string{"included.custom.audience": "ns/wl"},
			}})
		case path == "/admin/realms/kagenti/default-default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/admin/realms/kagenti/clients") && r.Method == http.MethodGet && r.URL.Query().Get("clientId") == "kagenti":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "plat-int", "clientId": "kagenti"}})
		case path == "/admin/realms/kagenti/clients/plat-int/default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		default:
			// Critical: any PUT to /clients/<uuid>/default-client-scopes/... where uuid != plat-int
			// would mean we attached to an unintended client.
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
		// AgentClientUUID intentionally empty
	})
	if err != nil {
		t.Fatal(err)
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
	if getMapperCalls != 1 {
		t.Fatalf("expected 1 GET mapper call (verify only, no ensureAudienceMapper for existing scopes), got %d", getMapperCalls)
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

// TestEnsureAudienceScope_MapperFailureForNewScope verifies that when a new scope is created
// but the initial mapper POST fails (500), verifyAudienceMapper repairs the missing mapper
// via createAudienceMapperBestEffort. The initial failure is non-fatal (matches Python sidecar
// which swallows mapper creation exceptions).
func TestEnsureAudienceScope_MapperFailureForNewScope(t *testing.T) {
	var postMapperCalls int
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

		// Mapper POST: first call (from ensureAudienceMapper) returns 500, second call
		// (from verifyAudienceMapper → createAudienceMapperBestEffort) succeeds
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodPost:
			postMapperCalls++
			if postMapperCalls == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal"}`))
			} else {
				w.WriteHeader(http.StatusCreated)
			}

		// GET mappers (from verifyAudienceMapper): returns empty list to trigger re-creation
		case strings.Contains(path, "/client-scopes/new-scope-id/protocol-mappers/models") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]protocolMapperRep{})

		// Realm default scope
		case path == "/admin/realms/kagenti/default-default-client-scopes/new-scope-id" && r.Method == http.MethodPut:
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
		t.Fatalf("expected success (verifyAudienceMapper repairs), got: %s", err)
	}
	if postMapperCalls != 2 {
		t.Fatalf("expected 2 POST mapper calls (initial fail + verify repair), got %d", postMapperCalls)
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

// TestEnsureAudienceScope_DeletesCorruptedMapper verifies that when a mapper exists with
// the correct name but the wrong ProtocolMapper type (corrupted state from issue #358),
// the operator deletes the stale mapper and re-creates the correct oidc-audience-mapper.
func TestEnsureAudienceScope_DeletesCorruptedMapper(t *testing.T) {
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

		// ensureAudienceMapper POST — 409 conflict (name collision with corrupted mapper)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodPost:
			if deleteMapperCalls > 0 {
				// After deletion, re-create succeeds
				recreatePostCalls++
				w.WriteHeader(http.StatusCreated)
			} else {
				w.WriteHeader(http.StatusConflict)
			}

		// GET mappers — returns mapper with wrong type (corrupted)
		case strings.Contains(path, "/client-scopes/scope-123/protocol-mappers/models") && r.Method == http.MethodGet:
			if deleteMapperCalls > 0 {
				// After delete+recreate, verify sees the correct mapper
				_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
					ID: "mapper-new", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
					ProtocolMapper: "oidc-audience-mapper",
					Config:         map[string]string{"included.custom.audience": spiffeURI},
				}})
			} else {
				// Corrupted: same name, wrong ProtocolMapper type
				_ = json.NewEncoder(w).Encode([]protocolMapperRep{{
					ID: "mapper-corrupted", Name: "agent-ns-wl-aud", Protocol: "openid-connect",
					ProtocolMapper: "oidc-usermodel-attribute-mapper", // wrong type!
					Config:         map[string]string{"claim.name": "audience"},
				}})
			}

		// DELETE the corrupted mapper
		case strings.Contains(path, "/protocol-mappers/models/mapper-corrupted") && r.Method == http.MethodDelete:
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
		t.Fatalf("expected 1 DELETE for corrupted mapper, got %d", deleteMapperCalls)
	}
	if recreatePostCalls != 1 {
		t.Fatalf("expected 1 POST to recreate correct mapper, got %d", recreatePostCalls)
	}
}

func TestEnsureAudienceScope_Disabled(t *testing.T) {
	a := Admin{}
	err := a.EnsureAudienceScope(context.Background(), "t", AudienceParams{AudienceScopeEnabled: false})
	if err != nil {
		t.Fatal(err)
	}
}
