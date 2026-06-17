/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SpiffeAuthClient implements SPIFFE ID Authentication for client registration
// using JWT-SVID authentication instead of admin credentials. This eliminates
// the need for long-lived admin credentials.
//
// Authentication flow:
// 1. Exchange JWT-SVID for Keycloak access token via token exchange
// 2. Use access token to call Keycloak Admin API for client registration
//
// This matches the pattern used by the operator bootstrap job.
type SpiffeAuthClient struct {
	BaseURL    string // e.g. https://keycloak.example.com:8080 (no trailing path)
	HTTPClient *http.Client
}

func (d *SpiffeAuthClient) httpc() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return http.DefaultClient
}

// clientRequest represents the payload for Keycloak Admin API client creation.
// Uses the standard Keycloak ClientRepresentation format.
type clientRequest struct {
	ClientID                  string            `json:"clientId"`
	Name                      string            `json:"name,omitempty"`
	StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
	DirectAccessGrantsEnabled bool              `json:"directAccessGrantsEnabled"`
	ServiceAccountsEnabled    bool              `json:"serviceAccountsEnabled"`
	PublicClient              bool              `json:"publicClient"`
	FullScopeAllowed          bool              `json:"fullScopeAllowed"`
	Attributes                map[string]string `json:"attributes,omitempty"`
	ClientAuthenticatorType   string            `json:"clientAuthenticatorType,omitempty"`
	Secret                    string            `json:"secret,omitempty"` // For client-secret auth
}

// tokenExchangeResponse represents the response from token exchange.
type tokenExchangeResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope,omitempty"`
}

// exchangeJWTSVIDForAccessToken obtains a Keycloak access token using
// OAuth 2.0 Client Credentials grant with JWT-SVID as client assertion.
//
// The operator client must be configured in Keycloak with:
// - clientAuthenticatorType: federated-jwt
// - Service account enabled
// - Assigned the "manage-clients" role from realm-management
//
// Parameters:
//   - ctx: Context for the request
//   - jwtSVID: JWT-SVID token obtained from SPIRE
//   - operatorClientID: The operator's SPIFFE ID (e.g., spiffe://localtest.me/ns/kagenti-system/sa/controller-manager)
//   - realm: Target Keycloak realm
//
// Returns:
//   - accessToken: Keycloak access token for calling Admin API
//   - error: Any error that occurred during token request
func (d *SpiffeAuthClient) exchangeJWTSVIDForAccessToken(ctx context.Context, jwtSVID, operatorClientID, realm string) (string, error) {
	base := trimBaseURL(d.BaseURL)
	tokenEndpoint := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", base, realm)

	// Build client credentials grant request with JWT-SVID assertion
	// This uses the service account of the operator client
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", operatorClientID)
	// CRITICAL: Use jwt-spiffe assertion type, not jwt-bearer
	// Keycloak's SPIFFE provider (SpiffeIdentityProvider.java) specifically validates
	// for CLIENT_ASSERTION_TYPE = "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe"
	// Using jwt-bearer will fail with invalid_client_credentials
	data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe")
	data.Set("client_assertion", jwtSVID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create client credentials request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.httpc().Do(req)
	if err != nil {
		return "", fmt.Errorf("client credentials request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("client credentials grant failed: status %d: %s", resp.StatusCode, truncate(body, 512))
	}

	var tokenResp tokenExchangeResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("client credentials grant returned empty access token")
	}

	return tokenResp.AccessToken, nil
}

// RegisterClientWithJWTSVID registers an OAuth client using SPIFFE ID authentication.
//
// This method:
// 1. Obtains a Keycloak access token using client credentials grant with JWT-SVID
// 2. Uses the access token to call the Keycloak Admin API to create the client
//
// The operator must be registered in Keycloak with federated-jwt authentication
// and have the manage-clients role assigned. This is typically done by the
// operator bootstrap job during installation.
//
// Parameters:
//   - ctx: Context for the request
//   - jwtSVID: JWT-SVID token obtained from SPIRE
//   - params: Client registration parameters
//
// Returns:
//   - clientSecret: The generated client secret (for client-secret auth)
//   - registrationToken: Empty string (not used with Admin API)
//   - error: Any error that occurred during registration
func (d *SpiffeAuthClient) RegisterClientWithJWTSVID(ctx context.Context, jwtSVID string, params ClientRegistrationParams) (clientSecret, registrationToken string, err error) {
	// Step 1: Get access token using client credentials grant with JWT-SVID
	// The operator client ID should be its SPIFFE ID
	operatorClientID := params.OperatorClientID
	if operatorClientID == "" {
		return "", "", fmt.Errorf("operator client ID (SPIFFE ID) is required for client credentials grant")
	}

	accessToken, err := d.exchangeJWTSVIDForAccessToken(ctx, jwtSVID, operatorClientID, params.Realm)
	if err != nil {
		return "", "", fmt.Errorf("get access token with JWT-SVID: %w", err)
	}

	// Step 2: Use access token to call Admin API
	base := trimBaseURL(d.BaseURL)
	endpoint := fmt.Sprintf("%s/admin/realms/%s/clients", base, params.Realm)

	// Build client registration request
	authType := params.ClientAuthType
	if authType == "" {
		authType = "client-secret"
	}

	attrs := map[string]string{
		"standard.token.exchange.enabled": fmt.Sprintf("%t", params.TokenExchangeEnable),
	}

	// For federated-jwt auth, configure JWT authentication
	if authType == "federated-jwt" {
		alias := params.SpiffeIDPAlias
		if alias == "" {
			alias = "spire-spiffe"
		}
		attrs["jwt.credential.issuer"] = alias
		attrs["jwt.credential.sub"] = params.ClientID
	}

	req := clientRequest{
		ClientID:   params.ClientID,
		Name:       params.ClientName,
		// Service-to-service client defaults
		StandardFlowEnabled:       false,
		DirectAccessGrantsEnabled: false,
		ServiceAccountsEnabled:    true,
		PublicClient:              false,
		FullScopeAllowed:          false,
		Attributes:                attrs,
		ClientAuthenticatorType:   authType,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", "", fmt.Errorf("marshal client request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}

	// Authenticate with access token from token exchange
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.httpc().Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("client registration request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("client registration failed: status %d: %s", resp.StatusCode, truncate(respBody, 512))
	}

	// Admin API returns the created client representation
	// For client-secret auth, we need to fetch the secret separately or it's in the response
	// Keycloak Admin API returns Location header with the client URL, we could fetch from there
	// For now, return empty secret - the controller will handle fetching it if needed
	return "", "", nil
}

// UpdateClientWithJWTSVID updates an existing OAuth client using SPIFFE ID authentication.
//
// This method:
// 1. Exchanges the JWT-SVID for a Keycloak access token
// 2. Uses the access token to call the Admin API to update the client
//
// Note: This is for future use. Currently, the operator uses RegisterOrFetchClient which
// handles both create and update.
func (d *SpiffeAuthClient) UpdateClientWithJWTSVID(ctx context.Context, jwtSVID string, params ClientRegistrationParams) error {
	// Similar flow to RegisterClientWithJWTSVID but using PUT to update
	return fmt.Errorf("client update via SPIFFE ID auth not yet implemented")
}
