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
	"strings"
)

// DCRClient implements Dynamic Client Registration using JWT-SVID authentication
// instead of admin credentials. This eliminates the need for long-lived admin
// credentials and reduces the security surface to DCR-only permissions.
//
// Keycloak DCR endpoint: POST /realms/{realm}/clients-registrations/default
// Authentication: Bearer <JWT-SVID>
//
// See: https://www.keycloak.org/docs/latest/securing_apps/#_client_registration
type DCRClient struct {
	BaseURL    string // e.g. https://keycloak.example.com:8080 (no trailing path)
	HTTPClient *http.Client
}

func (d *DCRClient) httpc() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return http.DefaultClient
}

// dcrRequest represents the payload for Keycloak DCR endpoint.
// This is a subset of the full client representation, focusing on the fields
// needed for operator-managed client registration.
type dcrRequest struct {
	ClientID                  string            `json:"clientId"`
	ClientName                string            `json:"clientName,omitempty"`
	RedirectURIs              []string          `json:"redirectUris,omitempty"`
	GrantTypes                []string          `json:"grantTypes,omitempty"`
	ResponseTypes             []string          `json:"responseTypes,omitempty"`
	Attributes                map[string]string `json:"attributes,omitempty"`
	ClientAuthenticatorType   string            `json:"clientAuthenticatorType,omitempty"`
}

// dcrResponse represents the response from Keycloak DCR endpoint.
type dcrResponse struct {
	ClientID       string `json:"clientId"`
	ClientSecret   string `json:"clientSecret,omitempty"`
	RegistrationAccessToken string `json:"registrationAccessToken"`
	ClientIDIssuedAt int64 `json:"clientIdIssuedAt"`
}

// RegisterClientWithJWTSVID registers an OAuth client using Dynamic Client Registration
// with JWT-SVID authentication instead of admin credentials.
//
// Parameters:
//   - ctx: Context for the request
//   - jwtSVID: JWT-SVID token obtained from SPIRE
//   - params: Client registration parameters
//
// Returns:
//   - clientSecret: The generated client secret (for client-secret auth)
//   - registrationToken: Registration access token for future updates
//   - error: Any error that occurred during registration
func (d *DCRClient) RegisterClientWithJWTSVID(ctx context.Context, jwtSVID string, params ClientRegistrationParams) (clientSecret, registrationToken string, err error) {
	base := trimBaseURL(d.BaseURL)
	endpoint := fmt.Sprintf("%s/realms/%s/clients-registrations/default", base, params.Realm)

	// Build DCR request
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

	req := dcrRequest{
		ClientID:   params.ClientID,
		ClientName: params.ClientName,
		// DCR defaults for service-to-service auth
		GrantTypes:    []string{"client_credentials", "urn:ietf:params:oauth:grant-type:token-exchange"},
		ResponseTypes: []string{"token"},
		Attributes:    attrs,
		ClientAuthenticatorType: authType,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", "", fmt.Errorf("marshal DCR request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}

	// Authenticate with JWT-SVID
	httpReq.Header.Set("Authorization", "Bearer "+jwtSVID)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.httpc().Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("DCR request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("DCR failed: status %d: %s", resp.StatusCode, truncate(respBody, 512))
	}

	var dcrResp dcrResponse
	if err := json.Unmarshal(respBody, &dcrResp); err != nil {
		return "", "", fmt.Errorf("decode DCR response: %w", err)
	}

	return dcrResp.ClientSecret, dcrResp.RegistrationAccessToken, nil
}

// UpdateClientWithJWTSVID updates an existing OAuth client using the registration access token.
//
// Note: This is for future use. Currently, the operator uses RegisterOrFetchClient which
// handles both create and update. For DCR, we need to store the registrationAccessToken
// and use it for updates instead of admin credentials.
func (d *DCRClient) UpdateClientWithJWTSVID(ctx context.Context, registrationToken string, params ClientRegistrationParams) error {
	// Implementation for update using PUT /realms/{realm}/clients-registrations/default/{clientId}
	// with Authorization: Bearer <registrationAccessToken>
	return fmt.Errorf("DCR update not yet implemented")
}
