/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package spire

import (
	"context"
	"fmt"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Client wraps the SPIRE Workload API client for obtaining JWT-SVIDs.
type Client struct {
	// SocketPath is the path to the SPIRE Agent's workload API socket.
	// Default: "unix:///run/spire/sockets/agent.sock"
	SocketPath string

	// client is the underlying workloadapi.Client
	client *workloadapi.Client
}

// NewClient creates a new SPIRE client.
func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = "unix:///run/spire/sockets/agent.sock"
	}
	return &Client{
		SocketPath: socketPath,
	}
}

// Connect establishes a connection to the SPIRE Agent.
// This should be called once during operator startup.
func (c *Client) Connect(ctx context.Context) error {
	client, err := workloadapi.New(ctx, workloadapi.WithAddr(c.SocketPath))
	if err != nil {
		return fmt.Errorf("connect to SPIRE workload API at %s: %w", c.SocketPath, err)
	}
	c.client = client
	return nil
}

// Close closes the connection to the SPIRE Agent.
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// FetchJWTSVID fetches a JWT-SVID from the SPIRE Agent for the given audience.
//
// The audience should be the Keycloak realm, e.g. "http://keycloak.localtest.me:8080/realms/kagenti"
// The returned JWT-SVID is a signed JWT token that can be used to authenticate with Keycloak.
//
// Parameters:
//   - ctx: Context for the request
//   - audience: The audience for the JWT-SVID (typically the Keycloak realm URL)
//
// Returns:
//   - jwtToken: The JWT-SVID token as a string
//   - expiresAt: When the JWT-SVID expires
//   - error: Any error that occurred
func (c *Client) FetchJWTSVID(ctx context.Context, audience string) (jwtToken string, expiresAt time.Time, err error) {
	if c.client == nil {
		return "", time.Time{}, fmt.Errorf("SPIRE client not connected")
	}

	// Fetch JWT-SVID with the specified audience
	params := jwtsvid.Params{
		Audience: audience,
	}
	jwtSVID, err := c.client.FetchJWTSVID(ctx, params)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("fetch JWT-SVID: %w", err)
	}

	return jwtSVID.Marshal(), jwtSVID.Expiry, nil
}

// FetchJWTSVIDs fetches multiple JWT-SVIDs for different audiences.
// This is useful when the operator needs to authenticate with multiple services.
func (c *Client) FetchJWTSVIDs(ctx context.Context, audiences []string) (map[string]string, error) {
	if c.client == nil {
		return nil, fmt.Errorf("SPIRE client not connected")
	}

	result := make(map[string]string)
	for _, aud := range audiences {
		params := jwtsvid.Params{
			Audience: aud,
		}
		jwtSVID, err := c.client.FetchJWTSVID(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("fetch JWT-SVID for audience %s: %w", aud, err)
		}
		result[aud] = jwtSVID.Marshal()
	}

	return result, nil
}

// GetSPIFFEID returns the SPIFFE ID of the workload (the operator).
// This can be used for logging and debugging.
func (c *Client) GetSPIFFEID(ctx context.Context) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("SPIRE client not connected")
	}

	x509Context, err := c.client.FetchX509Context(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch X509 context: %w", err)
	}

	if len(x509Context.SVIDs) == 0 {
		return "", fmt.Errorf("no X509-SVIDs available")
	}

	return x509Context.SVIDs[0].ID.String(), nil
}
