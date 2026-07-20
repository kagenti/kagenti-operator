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

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AudienceParams configures audience client-scope management (mirrors AuthBridge client_registration.py).
type AudienceParams struct {
	Realm                string
	ClientName           string   // e.g. namespace/workload — used to derive scope name
	AudienceClientID     string   // OAuth clientId / SPIFFE ID used as custom audience in the mapper
	PlatformClientIDs    []string // Keycloak clientId strings (e.g. UI client), not internal UUIDs
	AudienceScopeEnabled bool     // when false, EnsureAudienceScope is a no-op

	// AgentClientUUID is the internal Keycloak UUID of the agent's own client. When non-empty,
	// the audience scope is explicitly attached to this client as a default-client-scope, so
	// tokens issued via client_credentials carry the audience claim. Without this, agents
	// registered before the realm-level default-default scope existed won't have it attached
	// (Keycloak only auto-attaches default-defaults to newly-created clients).
	AgentClientUUID string
}

type clientScopeListItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type clientScopeCreateRep struct {
	Name       string            `json:"name"`
	Protocol   string            `json:"protocol"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type protocolMapperRep struct {
	ID              string            `json:"id,omitempty"`
	Name            string            `json:"name"`
	Protocol        string            `json:"protocol"`
	ProtocolMapper  string            `json:"protocolMapper"`
	ConsentRequired bool              `json:"consentRequired"`
	Config          map[string]string `json:"config"`
}

const oidcAudienceMapper = "oidc-audience-mapper"

// AudienceScopeName derives the realm client-scope name from CLIENT_NAME (same as Python).
func AudienceScopeName(clientName string) string {
	return "agent-" + strings.ReplaceAll(clientName, "/", "-") + "-aud"
}

// EnsureAudienceScope creates or reuses an audience client scope, adds the oidc-audience mapper,
// registers it as a realm default default client scope, and attaches it to each platform client.
// Missing platform clients are skipped (like Python). Realm / per-client attachment errors are ignored
// except they are swallowed (Python prints only); attachment uses best-effort PUT with 204/409 success.
func (a *Admin) EnsureAudienceScope(ctx context.Context, token string, p AudienceParams) error {
	if !p.AudienceScopeEnabled {
		return nil
	}
	scopeName := AudienceScopeName(p.ClientName)
	scopeID, err := a.getOrCreateAudienceClientScope(ctx, token, p.Realm, scopeName, p.AudienceClientID)
	if err != nil {
		return err
	}
	if err := a.verifyAudienceMapper(ctx, token, p.Realm, scopeID, scopeName, p.AudienceClientID); err != nil {
		return fmt.Errorf("verify audience mapper for scope %q: %w", scopeName, err)
	}
	_ = a.putRealmDefaultDefaultClientScope(ctx, token, p.Realm, scopeID)
	// Attach the audience scope to the agent's own client. Required because Keycloak does
	// not retroactively apply realm default-default-client-scopes to clients that already exist
	// when the scope is added, and the agent's client is registered just before this call.
	if p.AgentClientUUID != "" {
		if err := a.putClientDefaultClientScope(ctx, token, p.Realm, p.AgentClientUUID, scopeID); err != nil {
			log.FromContext(ctx).V(1).Info("agent client scope attach failed",
				"clientUUID", p.AgentClientUUID, "scope", scopeName, "err", err.Error())
		}
	}
	for _, plat := range p.PlatformClientIDs {
		plat = strings.TrimSpace(plat)
		if plat == "" {
			continue
		}
		internal, err := a.findClientUUID(ctx, token, p.Realm, plat)
		if err != nil || internal == "" {
			continue
		}
		_ = a.putClientDefaultClientScope(ctx, token, p.Realm, internal, scopeID)
	}
	return nil
}

func (a *Admin) getOrCreateAudienceClientScope(ctx context.Context, token, realm, scopeName, audience string) (string, error) {
	scopeID, err := a.findClientScopeIDByName(ctx, token, realm, scopeName)
	if err != nil {
		return "", err
	}
	if scopeID != "" {
		// Scope already exists — do NOT touch its mappers here.
		// verifyAudienceMapper handles mapper verification via GET+PUT (never POST for existing scopes).
		// This matches the Python AuthBridge sidecar which only adds mappers during initial creation.
		return scopeID, nil
	}

	scopeID, err = a.createClientScope(ctx, token, realm, clientScopeCreateRep{
		Name:     scopeName,
		Protocol: "openid-connect",
		Attributes: map[string]string{
			"include.in.token.scope":    "true",
			"display.on.consent.screen": "true",
		},
	})
	if err != nil {
		return "", err
	}
	if scopeID == "" {
		return "", fmt.Errorf("create client scope %q returned empty id", scopeName)
	}
	if err := a.ensureAudienceMapper(ctx, token, realm, scopeID, scopeName, audience); err != nil {
		// Mapper creation failed for a brand-new scope. This can happen if createClientScope
		// hit a 409 race and another reconcile already created the mapper. Non-fatal —
		// verifyAudienceMapper will repair below in this reconcile.
		return scopeID, nil
	}
	return scopeID, nil
}

func (a *Admin) findClientScopeIDByName(ctx context.Context, token, realm, name string) (string, error) {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes"
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
		return "", fmt.Errorf("keycloak list client-scopes: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var list []clientScopeListItem
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("keycloak list client-scopes decode: %w", err)
	}
	for i := range list {
		if list[i].Name == name {
			return list[i].ID, nil
		}
	}
	return "", nil
}

func (a *Admin) createClientScope(ctx context.Context, token, realm string, rep clientScopeCreateRep) (string, error) {
	base := trimBaseURL(a.BaseURL)
	payload, err := json.Marshal(rep)
	if err != nil {
		return "", err
	}
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes"
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
		if loc := resp.Header.Get("Location"); loc != "" {
			if id := pathLastSegment(loc); id != "" {
				return id, nil
			}
		}
		return a.findClientScopeIDByName(ctx, token, realm, rep.Name)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		return a.findClientScopeIDByName(ctx, token, realm, rep.Name)
	}
	return "", fmt.Errorf("keycloak create client-scope: status %d: %s", resp.StatusCode, truncate(body, 512))
}

func (a *Admin) ensureAudienceMapper(ctx context.Context, token, realm, scopeID, scopeName, audience string) error {
	mapper := protocolMapperRep{
		Name:            scopeName,
		Protocol:        "openid-connect",
		ProtocolMapper:  oidcAudienceMapper,
		ConsentRequired: false,
		Config: map[string]string{
			"included.custom.audience": audience,
			"id.token.claim":           "false",
			"access.token.claim":       "true",
			"userinfo.token.claim":     "false",
		},
	}
	payload, err := json.Marshal(mapper)
	if err != nil {
		return err
	}
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes/" + url.PathEscape(scopeID) + "/protocol-mappers/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
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
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	if resp.StatusCode == http.StatusConflict {
		// Mapper already exists — check if its audience needs updating.
		return a.updateAudienceMapperIfNeeded(ctx, token, realm, scopeID, scopeName, audience)
	}
	body, _ := io.ReadAll(resp.Body)
	// Mapper may already exist — treat other errors as non-fatal (Python logs and continues).
	if resp.StatusCode >= 400 {
		return fmt.Errorf("keycloak add audience mapper: status %d: %s", resp.StatusCode, truncate(body, 256))
	}
	return nil
}

// listAudienceMappers fetches all protocol mappers for a client scope.
func (a *Admin) listAudienceMappers(ctx context.Context, token, realm, scopeID string) ([]protocolMapperRep, error) {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes/" + url.PathEscape(scopeID) + "/protocol-mappers/models"
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
		return nil, fmt.Errorf("keycloak list mappers: status %d: %s", resp.StatusCode, truncate(body, 256))
	}

	var mappers []protocolMapperRep
	if err := json.Unmarshal(body, &mappers); err != nil {
		return nil, fmt.Errorf("keycloak list mappers decode: %w", err)
	}
	return mappers, nil
}

// updateAudienceMapperIfNeeded fetches the existing mapper for the scope and updates
// its included.custom.audience if it differs from the desired value.
// If a mapper with the correct name exists but has the wrong ProtocolMapper type
// (e.g. corrupted state), it deletes the stale mapper and re-creates via best-effort POST.
// If no mapper is found at all (ghost 409 from Keycloak's name index), returns nil
// to allow verifyAudienceMapper to retry on the next reconcile.
func (a *Admin) updateAudienceMapperIfNeeded(ctx context.Context, token, realm, scopeID, scopeName, audience string) error {
	mappers, err := a.listAudienceMappers(ctx, token, realm, scopeID)
	if err != nil {
		return err
	}

	for i := range mappers {
		if mappers[i].Name != scopeName {
			continue
		}
		if mappers[i].ProtocolMapper != oidcAudienceMapper {
			if err := a.deleteMapper(ctx, token, realm, scopeID, mappers[i].ID); err != nil {
				return fmt.Errorf("delete stale mapper %q (type %q) for scope %q: %w",
					mappers[i].Name, mappers[i].ProtocolMapper, scopeName, err)
			}
			return a.createAudienceMapperBestEffort(ctx, token, realm, scopeID, scopeName, audience)
		}
		if mappers[i].Config == nil {
			continue
		}
		if mappers[i].Config["included.custom.audience"] == audience {
			return nil
		}
		mappers[i].Config["included.custom.audience"] = audience
		return a.putAudienceMapper(ctx, token, realm, scopeID, mappers[i])
	}
	// No mapper found despite 409 — Keycloak's internal name index is stale.
	// Return nil; verifyAudienceMapper will retry on next reconcile.
	return nil
}

func (a *Admin) deleteMapper(ctx context.Context, token, realm, scopeID, mapperID string) error {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes/" + url.PathEscape(scopeID) + "/protocol-mappers/models/" + url.PathEscape(mapperID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpc().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("keycloak delete mapper: status %d: %s", resp.StatusCode, truncate(body, 256))
}

// createAudienceMapperBestEffort posts a new audience mapper. On 409 (conflict), it verifies
// the mapper actually exists with the correct audience — if it does, that's fine (another
// reconcile got there first). If 409 but no mapper is visible (Keycloak ghost-conflict),
// it returns nil and the reconciler will retry on the next pass.
func (a *Admin) createAudienceMapperBestEffort(ctx context.Context, token, realm, scopeID, scopeName, audience string) error {
	mapper := protocolMapperRep{
		Name:            scopeName,
		Protocol:        "openid-connect",
		ProtocolMapper:  oidcAudienceMapper,
		ConsentRequired: false,
		Config: map[string]string{
			"included.custom.audience": audience,
			"id.token.claim":           "false",
			"access.token.claim":       "true",
			"userinfo.token.claim":     "false",
		},
	}
	payload, err := json.Marshal(mapper)
	if err != nil {
		return err
	}
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes/" + url.PathEscape(scopeID) + "/protocol-mappers/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusCreated || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	if resp.StatusCode == http.StatusConflict {
		// 409 can mean: (a) another reconcile created it already, or (b) Keycloak ghost index.
		// Verify via GET — if the mapper exists with correct audience, success.
		mappers, err := a.listAudienceMappers(ctx, token, realm, scopeID)
		if err != nil {
			return nil
		}
		for i := range mappers {
			if mappers[i].Name == scopeName && mappers[i].ProtocolMapper == oidcAudienceMapper {
				if mappers[i].Config != nil && mappers[i].Config["included.custom.audience"] == audience {
					return nil
				}
				// Mapper exists but wrong audience — update it.
				if mappers[i].Config == nil {
					mappers[i].Config = make(map[string]string)
				}
				mappers[i].Config["included.custom.audience"] = audience
				return a.putAudienceMapper(ctx, token, realm, scopeID, mappers[i])
			}
		}
		// Ghost-409: mapper not visible. Return nil; next reconcile will retry.
		return nil
	}
	return fmt.Errorf("keycloak create mapper best-effort: status %d: %s", resp.StatusCode, truncate(body, 256))
}

func (a *Admin) putAudienceMapper(ctx context.Context, token, realm, scopeID string, mapper protocolMapperRep) error {
	payload, err := json.Marshal(mapper)
	if err != nil {
		return err
	}
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes/" + url.PathEscape(scopeID) + "/protocol-mappers/models/" + url.PathEscape(mapper.ID)
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
	if resp.StatusCode == http.StatusNoContent || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("keycloak update audience mapper: status %d: %s", resp.StatusCode, truncate(body, 256))
}

// verifyAudienceMapper is a defense-in-depth check that runs on every reconcile.
// It GETs the mappers for a scope and ensures the oidc-audience-mapper exists with the
// correct audience. If the audience is stale, it PUTs an update. If a mapper with the
// correct name but wrong type exists, it deletes and re-creates. If the mapper is missing
// entirely, it attempts a POST but treats 409 as success (avoids ghost-409 cascades).
func (a *Admin) verifyAudienceMapper(ctx context.Context, token, realm, scopeID, scopeName, audience string) error {
	mappers, err := a.listAudienceMappers(ctx, token, realm, scopeID)
	if err != nil {
		return err
	}

	for i := range mappers {
		if mappers[i].Name != scopeName {
			continue
		}
		if mappers[i].ProtocolMapper != oidcAudienceMapper {
			if err := a.deleteMapper(ctx, token, realm, scopeID, mappers[i].ID); err != nil {
				return fmt.Errorf("delete stale mapper %q (type %q): %w",
					mappers[i].Name, mappers[i].ProtocolMapper, err)
			}
			return a.createAudienceMapperBestEffort(ctx, token, realm, scopeID, scopeName, audience)
		}
		if mappers[i].Config != nil && mappers[i].Config["included.custom.audience"] == audience {
			return nil
		}
		if mappers[i].Config == nil {
			mappers[i].Config = make(map[string]string)
		}
		mappers[i].Config["included.custom.audience"] = audience
		return a.putAudienceMapper(ctx, token, realm, scopeID, mappers[i])
	}
	// Mapper not found — create it. Treat 409 as success (Keycloak name-index ghost).
	return a.createAudienceMapperBestEffort(ctx, token, realm, scopeID, scopeName, audience)
}

func (a *Admin) putRealmDefaultDefaultClientScope(ctx context.Context, token, realm, scopeID string) error {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/default-default-client-scopes/" + url.PathEscape(scopeID)
	return a.putNoBodyExpectSuccess(ctx, token, endpoint)
}

func (a *Admin) putClientDefaultClientScope(ctx context.Context, token, realm, clientInternalUUID, scopeID string) error {
	base := trimBaseURL(a.BaseURL)
	endpoint := base + "/admin/realms/" + url.PathEscape(realm) + "/clients/" + url.PathEscape(clientInternalUUID) + "/default-client-scopes/" + url.PathEscape(scopeID)
	return a.putNoBodyExpectSuccess(ctx, token, endpoint)
}

func (a *Admin) putNoBodyExpectSuccess(ctx context.Context, token, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpc().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// 204 success; 409 often means already linked; 404 can mean already removed / wrong id — ignore like Python prints.
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("keycloak PUT %s: status %d: %s", endpoint, resp.StatusCode, truncate(body, 256))
}
