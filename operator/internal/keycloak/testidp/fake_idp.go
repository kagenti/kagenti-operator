/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package testidp provides a configurable httptest server that simulates both
// the Keycloak admin REST API and a workload-facing OIDC token endpoint.
// It is intended for use in unit and integration tests across the operator.
package testidp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ClientEntry represents a registered Keycloak client.
type ClientEntry struct {
	InternalID string
	ClientID   string
	Name       string
	Secret     string
	Attributes map[string]any
}

// IssuedToken tracks a token issued by the fake IdP.
type IssuedToken struct {
	Token     string
	ClientID  string
	ExpiresAt time.Time
}

// Option configures a FakeIDP before starting.
type Option func(*FakeIDP)

// WithAdminCredentials sets the admin username/password accepted by the master realm token endpoint.
func WithAdminCredentials(user, pass string) Option {
	return func(f *FakeIDP) {
		f.adminUser = user
		f.adminPass = pass
	}
}

// WithTokenTTL sets the default access token TTL for both admin and workload tokens.
func WithTokenTTL(d time.Duration) Option {
	return func(f *FakeIDP) { f.tokenTTL = d }
}

// WithRealm sets the realm name used in URL paths (default: "rossoctl").
func WithRealm(realm string) Option {
	return func(f *FakeIDP) { f.realm = realm }
}

// WithErrorMode makes the server return the given HTTP status on all requests
// until ClearErrorMode is called. Useful for simulating outages.
func WithErrorMode(statusCode int) Option {
	return func(f *FakeIDP) { f.errorMode = statusCode }
}

// FakeIDP is a test Keycloak server that handles admin REST, client_credentials grants,
// and token introspection.
type FakeIDP struct {
	Server *httptest.Server

	mu         sync.Mutex
	realm      string
	adminUser  string
	adminPass  string
	tokenTTL   time.Duration
	errorMode  int
	clients    map[string]*ClientEntry  // keyed by clientId
	tokens     map[string]*IssuedToken  // keyed by token string
	scopes     map[string]scopeEntry    // keyed by scope ID
	mappers    map[string][]mapperEntry // protocol mappers keyed by scope ID
	nextUUID   int
	tokenCalls int
	t          testing.TB
}

type scopeEntry struct {
	ID   string
	Name string
}

type mapperEntry struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

// Start creates and starts a new FakeIDP. Call Close() when done.
func Start(t testing.TB, opts ...Option) *FakeIDP {
	t.Helper()
	f := &FakeIDP{
		realm:     "rossoctl",
		adminUser: "admin",
		adminPass: "admin",
		tokenTTL:  time.Hour,
		clients:   make(map[string]*ClientEntry),
		tokens:    make(map[string]*IssuedToken),
		scopes:    make(map[string]scopeEntry),
		mappers:   make(map[string][]mapperEntry),
		t:         t,
	}
	for _, o := range opts {
		o(f)
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.Server.Close)
	return f
}

// URL returns the base URL of the fake server.
func (f *FakeIDP) URL() string { return f.Server.URL }

// Client returns the httptest server's HTTP client (handles TLS correctly).
func (f *FakeIDP) Client() *http.Client { return f.Server.Client() }

// SetErrorMode makes all subsequent requests return the given status code.
func (f *FakeIDP) SetErrorMode(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorMode = code
}

// ClearErrorMode returns the server to normal operation.
func (f *FakeIDP) ClearErrorMode() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorMode = 0
}

// SetTokenTTL changes the TTL for newly issued tokens.
func (f *FakeIDP) SetTokenTTL(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenTTL = d
}

// TokenHTTPCalls returns how many token endpoint HTTP requests were received.
func (f *FakeIDP) TokenHTTPCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// RegisterClient pre-registers a client so admin operations find it.
func (f *FakeIDP) RegisterClient(clientID, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUUID++
	f.clients[clientID] = &ClientEntry{
		InternalID: fmt.Sprintf("uuid-%d", f.nextUUID),
		ClientID:   clientID,
		Name:       clientID,
		Secret:     secret,
		Attributes: map[string]any{"standard.token.exchange.enabled": []any{"false"}},
	}
}

// GetClient returns a registered client, or nil.
func (f *FakeIDP) GetClient(clientID string) *ClientEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[clientID]
}

// ValidateToken checks whether a token string is valid (exists and not expired).
func (f *FakeIDP) ValidateToken(token string) (clientID string, valid bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[token]
	if !ok {
		return "", false
	}
	if time.Now().After(t.ExpiresAt) {
		delete(f.tokens, token)
		return t.ClientID, false
	}
	return t.ClientID, true
}

func (f *FakeIDP) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	errMode := f.errorMode
	f.mu.Unlock()
	if errMode != 0 {
		http.Error(w, "simulated error", errMode)
		return
	}

	path := r.URL.Path
	switch {
	// Master realm token endpoint (admin password grant)
	case path == "/realms/master/protocol/openid-connect/token" && r.Method == http.MethodPost:
		f.handleAdminToken(w, r)

	// Workload token endpoint (client_credentials grant)
	case path == fmt.Sprintf("/realms/%s/protocol/openid-connect/token", f.realm) && r.Method == http.MethodPost:
		f.handleWorkloadToken(w, r)

	// Token introspection
	case path == fmt.Sprintf("/realms/%s/protocol/openid-connect/token/introspect", f.realm) && r.Method == http.MethodPost:
		f.handleIntrospect(w, r)

	// Client list by clientId query param
	case strings.HasPrefix(path, fmt.Sprintf("/admin/realms/%s/clients", f.realm)) && r.Method == http.MethodGet && r.URL.Query().Get("clientId") != "":
		f.handleListClients(w, r)

	// Client secret
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/client-secret"):
		f.handleClientSecret(w, r)

	// GET single client
	case r.Method == http.MethodGet && f.isClientUUIDPath(path):
		f.handleGetClient(w, r)

	// PUT single client (drift reconciliation)
	case r.Method == http.MethodPut && f.isClientUUIDPath(path):
		f.handleUpdateClient(w, r)

	// POST create client
	case path == fmt.Sprintf("/admin/realms/%s/clients", f.realm) && r.Method == http.MethodPost:
		f.handleCreateClient(w, r)

	// Client scopes list
	case path == fmt.Sprintf("/admin/realms/%s/client-scopes", f.realm) && r.Method == http.MethodGet:
		f.handleListClientScopes(w, r)

	// Client scope create
	case path == fmt.Sprintf("/admin/realms/%s/client-scopes", f.realm) && r.Method == http.MethodPost:
		f.handleCreateClientScope(w, r)

	// Protocol mapper list (GET)
	case r.Method == http.MethodGet && strings.Contains(path, "/protocol-mappers/models"):
		f.handleListProtocolMappers(w, r)

	// Protocol mapper create (audience mapper)
	case r.Method == http.MethodPost && strings.Contains(path, "/protocol-mappers/models"):
		f.handleCreateProtocolMapper(w, r)

	// Realm default scope / client default scope PUTs
	case r.Method == http.MethodPut && (strings.Contains(path, "/default-default-client-scopes/") || strings.Contains(path, "/default-client-scopes/")):
		w.WriteHeader(http.StatusNoContent)

	default:
		f.t.Errorf("FakeIDP: unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func (f *FakeIDP) handleAdminToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.tokenCalls++
	ttl := f.tokenTTL
	user := f.adminUser
	pass := f.adminPass
	f.mu.Unlock()

	if r.FormValue("username") != user || r.FormValue("password") != pass {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusUnauthorized)
		return
	}

	tok := f.issueToken("admin-cli", ttl)
	writeJSON(w, map[string]any{
		"access_token": tok,
		"expires_in":   int(ttl.Seconds()),
		"token_type":   "Bearer",
	})
}

func (f *FakeIDP) handleWorkloadToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.FormValue("grant_type") != "client_credentials" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}

	clientID := r.FormValue("client_id")
	clientSecret := r.FormValue("client_secret")

	f.mu.Lock()
	entry, ok := f.clients[clientID]
	ttl := f.tokenTTL
	f.tokenCalls++
	f.mu.Unlock()

	if !ok || entry.Secret != clientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}

	tok := f.issueToken(clientID, ttl)
	writeJSON(w, map[string]any{
		"access_token": tok,
		"expires_in":   int(ttl.Seconds()),
		"token_type":   "Bearer",
	})
}

func (f *FakeIDP) handleIntrospect(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token := r.FormValue("token")

	clientID, valid := f.ValidateToken(token)
	if !valid {
		writeJSON(w, map[string]any{"active": false})
		return
	}
	writeJSON(w, map[string]any{
		"active":     true,
		"client_id":  clientID,
		"token_type": "Bearer",
	})
}

func (f *FakeIDP) handleListClients(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("clientId")
	f.mu.Lock()
	entry, ok := f.clients[cid]
	f.mu.Unlock()

	if !ok {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, []map[string]string{{"id": entry.InternalID, "clientId": entry.ClientID}})
}

func (f *FakeIDP) handleClientSecret(w http.ResponseWriter, r *http.Request) {
	uuid := f.uuidFromPath(r.URL.Path)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clients {
		if c.InternalID == uuid {
			writeJSON(w, map[string]string{"value": c.Secret})
			return
		}
	}
	http.NotFound(w, r)
}

func (f *FakeIDP) handleGetClient(w http.ResponseWriter, r *http.Request) {
	uuid := f.uuidFromPath(r.URL.Path)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clients {
		if c.InternalID == uuid {
			writeJSON(w, map[string]any{
				"id":                        c.InternalID,
				"clientId":                  c.ClientID,
				"name":                      c.Name,
				"standardFlowEnabled":       true,
				"directAccessGrantsEnabled": true,
				"serviceAccountsEnabled":    true,
				"fullScopeAllowed":          false,
				"publicClient":              false,
				"clientAuthenticatorType":   "client-secret",
				"attributes":                c.Attributes,
			})
			return
		}
	}
	http.NotFound(w, r)
}

func (f *FakeIDP) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	uuid := f.uuidFromPath(r.URL.Path)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clients {
		if c.InternalID == uuid {
			if attrs, ok := body["attributes"].(map[string]any); ok {
				c.Attributes = attrs
			}
			if name, ok := body["name"].(string); ok {
				c.Name = name
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.NotFound(w, r)
}

func (f *FakeIDP) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string `json:"clientId"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	if _, exists := f.clients[body.ClientID]; exists {
		f.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	f.nextUUID++
	secret := randomHex(16)
	entry := &ClientEntry{
		InternalID: fmt.Sprintf("uuid-%d", f.nextUUID),
		ClientID:   body.ClientID,
		Name:       body.Name,
		Secret:     secret,
		Attributes: map[string]any{},
	}
	f.clients[body.ClientID] = entry
	f.mu.Unlock()

	w.Header().Set("Location", fmt.Sprintf("%s/admin/realms/%s/clients/%s",
		f.Server.URL, f.realm, entry.InternalID))
	w.WriteHeader(http.StatusCreated)
}

func (f *FakeIDP) handleListClientScopes(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]map[string]string, 0, len(f.scopes))
	for _, s := range f.scopes {
		list = append(list, map[string]string{"id": s.ID, "name": s.Name})
	}
	if list == nil {
		list = []map[string]string{}
	}
	writeJSON(w, list)
}

func (f *FakeIDP) handleCreateClientScope(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.nextUUID++
	id := fmt.Sprintf("scope-%d", f.nextUUID)
	f.scopes[id] = scopeEntry{ID: id, Name: body.Name}
	f.mu.Unlock()

	w.Header().Set("Location", fmt.Sprintf("%s/admin/realms/%s/client-scopes/%s",
		f.Server.URL, f.realm, id))
	w.WriteHeader(http.StatusCreated)
}

func (f *FakeIDP) handleListProtocolMappers(w http.ResponseWriter, r *http.Request) {
	scopeID := f.scopeIDFromMappersPath(r.URL.Path)
	f.mu.Lock()
	mappers := f.mappers[scopeID]
	f.mu.Unlock()
	if mappers == nil {
		mappers = []mapperEntry{}
	}
	writeJSON(w, mappers)
}

func (f *FakeIDP) handleCreateProtocolMapper(w http.ResponseWriter, r *http.Request) {
	scopeID := f.scopeIDFromMappersPath(r.URL.Path)
	var body struct {
		Name           string            `json:"name"`
		Protocol       string            `json:"protocol"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.nextUUID++
	entry := mapperEntry{
		ID:             fmt.Sprintf("mapper-%d", f.nextUUID),
		Name:           body.Name,
		Protocol:       body.Protocol,
		ProtocolMapper: body.ProtocolMapper,
		Config:         body.Config,
	}
	f.mappers[scopeID] = append(f.mappers[scopeID], entry)
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (f *FakeIDP) scopeIDFromMappersPath(path string) string {
	prefix := fmt.Sprintf("/admin/realms/%s/client-scopes/", f.realm)
	rest := strings.TrimPrefix(path, prefix)
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// issueToken creates a token and stores it. Caller must NOT hold f.mu.
func (f *FakeIDP) issueToken(clientID string, ttl time.Duration) string {
	tok := randomHex(32)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[tok] = &IssuedToken{
		Token:     tok,
		ClientID:  clientID,
		ExpiresAt: time.Now().Add(ttl),
	}
	return tok
}

// WorkloadTokenEndpoint returns the full URL for the realm's token endpoint.
func (f *FakeIDP) WorkloadTokenEndpoint() string {
	return fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", f.Server.URL, f.realm)
}

// IntrospectionEndpoint returns the full URL for the realm's token introspection endpoint.
func (f *FakeIDP) IntrospectionEndpoint() string {
	return fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token/introspect", f.Server.URL, f.realm)
}

// isClientUUIDPath returns true if the path matches /admin/realms/{realm}/clients/{uuid}
// (not /client-secret, not a query-param list).
func (f *FakeIDP) isClientUUIDPath(path string) bool {
	prefix := fmt.Sprintf("/admin/realms/%s/clients/", f.realm)
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// uuidFromPath extracts the last path segment (e.g. uuid from .../clients/uuid or .../clients/uuid/client-secret).
func (f *FakeIDP) uuidFromPath(path string) string {
	// Strip trailing /client-secret if present
	path = strings.TrimSuffix(path, "/client-secret")
	path = strings.TrimRight(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestWorkloadToken performs a client_credentials grant against the fake IdP and returns the access token.
// This is a test helper — it simulates what the Auth Bridge sidecar does.
func (f *FakeIDP) RequestWorkloadToken(clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	resp, err := f.Client().Post(f.WorkloadTokenEndpoint(), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request failed: status %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.AccessToken, nil
}
