/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package keycloak

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientRegistrationParams mirrors the AuthBridge client-registration sidecar contract.
type ClientRegistrationParams struct {
	Realm               string
	ClientID            string // Keycloak OAuth clientId (SPIFFE ID or namespace/name)
	ClientName          string // Human-readable name field in Keycloak
	ClientAuthType      string // "client-secret" or "federated-jwt"
	SpiffeIDPAlias      string // Keycloak SPIFFE IdP alias when using federated-jwt
	TokenExchangeEnable bool
	OperatorClientID    string // Operator's SPIFFE ID for client credentials grant
}

type adminTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type keycloakClientRep struct {
	ID       string `json:"id,omitempty"`
	ClientID string `json:"clientId"`
	Name     string `json:"name"`

	StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
	DirectAccessGrantsEnabled bool              `json:"directAccessGrantsEnabled"`
	ServiceAccountsEnabled    bool              `json:"serviceAccountsEnabled"`
	FullScopeAllowed          bool              `json:"fullScopeAllowed"`
	PublicClient              bool              `json:"publicClient"`
	ClientAuthenticatorType   string            `json:"clientAuthenticatorType"`
	Attributes                map[string]string `json:"attributes"`
}

type clientSecretRep struct {
	Value string `json:"value"`
}

// Admin is a minimal Keycloak admin REST client (password grant, client CRUD, secret read).
type Admin struct {
	BaseURL    string // e.g. https://keycloak.example.com:8080 (no trailing path)
	HTTPClient *http.Client
}

func (a *Admin) httpc() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

func trimBaseURL(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// adminToken returns an access token and its absolute expiry time (from OAuth expires_in).
// If expires_in is absent or zero, a conservative default TTL is applied.
func (a *Admin) adminToken(ctx context.Context, username, password string) (string, time.Time, error) {
	base := trimBaseURL(a.BaseURL)
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", "admin-cli")
	form.Set("username", username)
	form.Set("password", password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/realms/master/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpc().Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("keycloak token: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var tr adminTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("keycloak token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("keycloak token: empty access_token")
	}
	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 60
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	return tr.AccessToken, expiresAt, nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// PasswordGrantToken returns an admin access token using the master realm password grant (admin-cli).
func (a *Admin) PasswordGrantToken(ctx context.Context, adminUser, adminPass string) (string, error) {
	token, _, err := a.adminToken(ctx, adminUser, adminPass)
	return token, err
}

// RegisterOrFetchClient ensures an OAuth client exists and returns its internal UUID and client secret value.
func (a *Admin) RegisterOrFetchClient(ctx context.Context, adminUser, adminPass string, p ClientRegistrationParams) (internalID, secret string, err error) {
	token, _, err := a.adminToken(ctx, adminUser, adminPass)
	if err != nil {
		return "", "", err
	}
	return a.RegisterOrFetchClientWithToken(ctx, token, p)
}

// RegisterOrFetchClientWithToken is like RegisterOrFetchClient but reuses an existing admin token.
func (a *Admin) RegisterOrFetchClientWithToken(ctx context.Context, token string, p ClientRegistrationParams) (internalID, secret string, err error) {
	authType := p.ClientAuthType
	if authType == "" {
		authType = "client-secret"
	}

	attrs := map[string]string{
		"standard.token.exchange.enabled": fmt.Sprintf("%t", p.TokenExchangeEnable),
	}
	if authType == "federated-jwt" {
		alias := p.SpiffeIDPAlias
		if alias == "" {
			alias = "spire-spiffe"
		}
		attrs["jwt.credential.issuer"] = alias
		attrs["jwt.credential.sub"] = p.ClientID
	}

	rep := keycloakClientRep{
		ClientID:                  p.ClientID,
		Name:                      p.ClientName,
		StandardFlowEnabled:       true,
		DirectAccessGrantsEnabled: true,
		ServiceAccountsEnabled:    true,
		FullScopeAllowed:          false,
		PublicClient:              false,
		ClientAuthenticatorType:   authType,
		Attributes:                attrs,
	}

	internalID, err = a.findClientUUID(ctx, token, p.Realm, p.ClientID)
	if err != nil {
		return "", "", err
	}
	if internalID == "" {
		internalID, err = a.createClient(ctx, token, p.Realm, &rep)
		if err != nil {
			return "", "", err
		}
	} else {
		if err := a.reconcileExistingClient(ctx, token, p.Realm, internalID, &rep); err != nil {
			return "", "", err
		}
	}

	secret, err = a.readClientSecret(ctx, token, p.Realm, internalID)
	if err != nil {
		return "", "", err
	}
	return internalID, secret, nil
}

func (a *Admin) findClientUUID(ctx context.Context, token, realm, clientID string) (string, error) {
	base := trimBaseURL(a.BaseURL)
	u, err := url.Parse(base + "/admin/realms/" + url.PathEscape(realm) + "/clients")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("clientId", clientID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpc().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak list clients: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var list []struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("keycloak list clients decode: %w", err)
	}
	for i := range list {
		if list[i].ClientID == clientID {
			return list[i].ID, nil
		}
	}
	return "", nil
}

func (a *Admin) createClient(ctx context.Context, token, realm string, rep *keycloakClientRep) (string, error) {
	base := trimBaseURL(a.BaseURL)
	payload, err := json.Marshal(rep)
	if err != nil {
		return "", err
	}
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/clients"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpc().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusCreated {
		loc := resp.Header.Get("Location")
		if loc != "" {
			if id := pathLastSegment(loc); id != "" {
				return id, nil
			}
		}
		// Fall through to lookup
		return a.findClientUUID(ctx, token, realm, rep.ClientID)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		return a.findClientUUID(ctx, token, realm, rep.ClientID)
	}
	return "", fmt.Errorf("keycloak create client: status %d: %s", resp.StatusCode, truncate(body, 512))
}

func pathLastSegment(loc string) string {
	loc = strings.TrimRight(loc, "/")
	if idx := strings.LastIndex(loc, "/"); idx >= 0 {
		return loc[idx+1:]
	}
	return ""
}

// reconcileExistingClient loads the Keycloak client and PUTs the representation when managed fields drift.
func (a *Admin) reconcileExistingClient(ctx context.Context, token, realm, internalUUID string, desired *keycloakClientRep) error {
	current, err := a.getClientRepresentationMap(ctx, token, realm, internalUUID)
	if err != nil {
		return err
	}
	if !clientRepDrifted(current, desired) {
		return nil
	}
	mergeDesiredClientIntoMap(current, desired)
	return a.updateClient(ctx, token, realm, internalUUID, current)
}

func (a *Admin) getClientRepresentationMap(ctx context.Context, token, realm, internalUUID string) (map[string]interface{}, error) {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/clients/" + url.PathEscape(internalUUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpc().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak get client: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("keycloak get client decode: %w", err)
	}
	return m, nil
}

func keycloakAttrString(attrs interface{}, key string) string {
	m, ok := attrs.(map[string]interface{})
	if !ok || m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []interface{}:
		if len(x) == 0 {
			return ""
		}
		if s, ok := x[0].(string); ok {
			return s
		}
	case []string:
		if len(x) > 0 {
			return x[0]
		}
	}
	return ""
}

func boolFromJSONMap(m map[string]interface{}, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	}
	return false
}

func stringFromJSONMap(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func clientRepDrifted(current map[string]interface{}, desired *keycloakClientRep) bool {
	if stringFromJSONMap(current, "name") != desired.Name {
		return true
	}
	if boolFromJSONMap(current, "standardFlowEnabled") != desired.StandardFlowEnabled {
		return true
	}
	if boolFromJSONMap(current, "directAccessGrantsEnabled") != desired.DirectAccessGrantsEnabled {
		return true
	}
	if boolFromJSONMap(current, "serviceAccountsEnabled") != desired.ServiceAccountsEnabled {
		return true
	}
	if boolFromJSONMap(current, "fullScopeAllowed") != desired.FullScopeAllowed {
		return true
	}
	if boolFromJSONMap(current, "publicClient") != desired.PublicClient {
		return true
	}
	if stringFromJSONMap(current, "clientAuthenticatorType") != desired.ClientAuthenticatorType {
		return true
	}
	attrs := current["attributes"]
	for k, want := range desired.Attributes {
		if keycloakAttrString(attrs, k) != want {
			return true
		}
	}
	return false
}

func mergeDesiredClientIntoMap(m map[string]interface{}, desired *keycloakClientRep) {
	m["clientId"] = desired.ClientID
	m["name"] = desired.Name
	m["standardFlowEnabled"] = desired.StandardFlowEnabled
	m["directAccessGrantsEnabled"] = desired.DirectAccessGrantsEnabled
	m["serviceAccountsEnabled"] = desired.ServiceAccountsEnabled
	m["fullScopeAllowed"] = desired.FullScopeAllowed
	m["publicClient"] = desired.PublicClient
	m["clientAuthenticatorType"] = desired.ClientAuthenticatorType

	var attrs map[string]interface{}
	if raw, ok := m["attributes"].(map[string]interface{}); ok && raw != nil {
		attrs = raw
	} else {
		attrs = make(map[string]interface{})
		m["attributes"] = attrs
	}
	for k, v := range desired.Attributes {
		attrs[k] = v
	}
}

func (a *Admin) updateClient(ctx context.Context, token, realm, internalUUID string, rep map[string]interface{}) error {
	base := trimBaseURL(a.BaseURL)
	payload, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/clients/" + url.PathEscape(internalUUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpc().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("keycloak update client: status %d: %s", resp.StatusCode, truncate(body, 512))
}

func (a *Admin) readClientSecret(ctx context.Context, token, realm, internalUUID string) (string, error) {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/clients/" + url.PathEscape(internalUUID) + "/client-secret"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpc().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak client secret: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var cs clientSecretRep
	if err := json.Unmarshal(body, &cs); err != nil {
		return "", fmt.Errorf("keycloak client secret decode: %w", err)
	}
	return cs.Value, nil
}

// HTTPClientConfig configures optional TLS and transport for Keycloak admin HTTP clients.
// Use it with NewHTTPClient when Keycloak is served with a certificate from a private CA
// (set TLSConfig.RootCAs) or when you need a custom RoundTripper.
type HTTPClientConfig struct {
	// Timeout for the whole client. Zero means 60 seconds.
	Timeout time.Duration
	// Transport, if non-nil, is used as the client's RoundTripper. TLSConfig is ignored when
	// Transport is set; configure TLS on the transport yourself.
	Transport http.RoundTripper
	// TLSConfig is used when Transport is nil: a clone of http.DefaultTransport gets this TLS
	// config (for example RootCAs for a private CA). If nil, default TLS verification applies.
	TLSConfig *tls.Config
}

// NewHTTPClient returns an HTTP client suitable for Keycloak admin calls.
func NewHTTPClient(cfg HTTPClientConfig) *http.Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	var rt http.RoundTripper
	switch {
	case cfg.Transport != nil:
		rt = cfg.Transport
	case cfg.TLSConfig != nil:
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = cfg.TLSConfig.Clone()
		rt = tr
	default:
		rt = nil
	}
	return &http.Client{Timeout: timeout, Transport: rt}
}

// DefaultHTTPClient returns an HTTP client suitable for Keycloak admin calls.
func DefaultHTTPClient() *http.Client {
	return NewHTTPClient(HTTPClientConfig{})
}
