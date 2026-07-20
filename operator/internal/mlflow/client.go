/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package mlflow provides a minimal REST API client for MLflow experiment management.
// The client authenticates using a Kubernetes ServiceAccount token and sets the
// X-MLFLOW-WORKSPACE header to scope operations to a specific namespace/workspace.
package mlflow

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTokenPath is the projected SA token path in a pod.
	DefaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

	// WorkspaceHeader is the MLflow workspace header (namespace-based isolation).
	WorkspaceHeader = "X-MLFLOW-WORKSPACE"

	// DefaultTimeout is the default HTTP client timeout.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the default number of retry attempts for transient errors.
	DefaultMaxRetries = 3

	// DefaultRetryBaseDelay is the initial delay between retries (before jitter).
	DefaultRetryBaseDelay = 500 * time.Millisecond

	// maxBackoffDelay caps the exponential backoff.
	maxBackoffDelay = 30 * time.Second
)

// Client is a minimal MLflow REST API client for experiment management.
type Client struct {
	// BaseURL is the MLflow tracking server URL
	BaseURL string

	// TokenPath is the path to the SA token file. Defaults to DefaultTokenPath.
	TokenPath string

	// CAFile is an optional path to a PEM-encoded CA certificate bundle to
	// append to the system cert pool for TLS verification. Used on clusters
	// where the MLflow gateway uses a non-publicly-trusted certificate
	// (e.g., HyperShift ingress CA).
	CAFile string

	// HTTPClient is the HTTP client to use. If nil, a default client is created
	// on first use with Timeout applied. Must be set before the first request.
	HTTPClient *http.Client

	// Timeout is the HTTP client timeout. Defaults to DefaultTimeout (30s).
	// Must be set before the first request (ignored after the HTTP client is created).
	Timeout time.Duration

	// MaxRetries is the maximum number of retry attempts for transient errors
	// (network errors, HTTP 429, 5xx). Defaults to DefaultMaxRetries (3).
	// Set to a negative value to disable retries.
	MaxRetries int

	// RetryBaseDelay is the initial delay between retries before exponential
	// backoff and jitter are applied. Defaults to DefaultRetryBaseDelay (500ms).
	RetryBaseDelay time.Duration

	httpOnce sync.Once
}

// createExperimentRequest is the request body for POST /api/2.0/mlflow/experiments/create.
type createExperimentRequest struct {
	Name string `json:"name"`
}

// createExperimentResponse is the response body from experiments/create.
type createExperimentResponse struct {
	ExperimentID string `json:"experiment_id"`
}

// getExperimentByNameResponse is the response body from experiments/get-by-name.
type getExperimentByNameResponse struct {
	Experiment struct {
		ExperimentID   string `json:"experiment_id"`
		LifecycleStage string `json:"lifecycle_stage"`
	} `json:"experiment"`
}

// restoreExperimentRequest is the request body for POST /api/2.0/mlflow/experiments/restore.
type restoreExperimentRequest struct {
	ExperimentID string `json:"experiment_id"`
}

// mlflowError represents an MLflow API error response.
type mlflowError struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

func (e *mlflowError) Error() string {
	return fmt.Sprintf("mlflow: %s: %s", e.ErrorCode, e.Message)
}

// IsResourceAlreadyExists returns true if the error is a RESOURCE_ALREADY_EXISTS error.
func IsResourceAlreadyExists(err error) bool {
	var e *mlflowError
	if errors.As(err, &e) {
		return e.ErrorCode == "RESOURCE_ALREADY_EXISTS"
	}
	return false
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c *Client) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	if c.MaxRetries < 0 {
		return 0 // explicitly disabled
	}
	return DefaultMaxRetries
}

func (c *Client) retryBaseDelay() time.Duration {
	if c.RetryBaseDelay > 0 {
		return c.RetryBaseDelay
	}
	return DefaultRetryBaseDelay
}

func (c *Client) httpClient() (*http.Client, error) {
	var initErr error
	c.httpOnce.Do(func() {
		if c.HTTPClient == nil {
			transport, err := c.buildTransport()
			if err != nil {
				initErr = err
				return
			}
			c.HTTPClient = &http.Client{
				Timeout:   c.timeout(),
				Transport: transport,
			}
		}
	})
	if initErr != nil {
		return nil, initErr
	}
	if c.HTTPClient == nil {
		return nil, fmt.Errorf("http client initialization failed")
	}
	return c.HTTPClient, nil
}

func (c *Client) buildTransport() (http.RoundTripper, error) {
	if c.CAFile == "" {
		return nil, nil
	}
	caPEM, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA file %q: %w", c.CAFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file %q contains no valid PEM certificates", c.CAFile)
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: pool,
		},
	}, nil
}

func (c *Client) tokenPath() string {
	if c.TokenPath != "" {
		return c.TokenPath
	}
	return DefaultTokenPath
}

func (c *Client) readToken() (string, error) {
	data, err := os.ReadFile(c.tokenPath())
	if err != nil {
		return "", fmt.Errorf("reading SA token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// CreateExperiment creates an MLflow experiment and returns the experiment ID.
// If the experiment already exists, it falls back to GetExperimentByName.
func (c *Client) CreateExperiment(ctx context.Context, name, workspace string) (string, error) {
	body, err := json.Marshal(createExperimentRequest{Name: name})
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	result, err := doJSONRequest[createExperimentResponse](c, ctx, http.MethodPost,
		"/api/2.0/mlflow/experiments/create", workspace, bytes.NewReader(body))
	if err != nil {
		if IsResourceAlreadyExists(err) {
			return c.getOrRestoreExperiment(ctx, name, workspace)
		}
		return "", err
	}
	return result.ExperimentID, nil
}

// GetExperimentByName retrieves an experiment by name and returns the experiment ID.
func (c *Client) GetExperimentByName(ctx context.Context, name, workspace string) (string, error) {
	id, _, err := c.getExperimentByName(ctx, name, workspace)
	return id, err
}

func (c *Client) getExperimentByName(ctx context.Context, name, workspace string) (string, string, error) {
	path := "/api/2.0/mlflow/experiments/get-by-name?experiment_name=" + url.QueryEscape(name)

	result, err := doJSONRequest[getExperimentByNameResponse](c, ctx, http.MethodGet, path, workspace, nil)
	if err != nil {
		return "", "", err
	}
	return result.Experiment.ExperimentID, result.Experiment.LifecycleStage, nil
}

// getOrRestoreExperiment fetches an existing experiment and restores it if deleted.
func (c *Client) getOrRestoreExperiment(ctx context.Context, name, workspace string) (string, error) {
	id, lifecycle, err := c.getExperimentByName(ctx, name, workspace)
	if err != nil {
		return "", err
	}
	if lifecycle == "deleted" {
		if err := c.restoreExperiment(ctx, id, workspace); err != nil {
			return "", fmt.Errorf("restoring deleted experiment %s: %w", id, err)
		}
	}
	return id, nil
}

// restoreExperiment restores a deleted MLflow experiment.
func (c *Client) restoreExperiment(ctx context.Context, experimentID, workspace string) error {
	body, err := json.Marshal(restoreExperimentRequest{ExperimentID: experimentID})
	if err != nil {
		return fmt.Errorf("marshaling restore request: %w", err)
	}
	return c.doRequestExpectOK(ctx, http.MethodPost, "/api/2.0/mlflow/experiments/restore", workspace, bytes.NewReader(body))
}

// doJSONRequest executes an HTTP request and decodes the JSON response.
func doJSONRequest[T any](c *Client, ctx context.Context, method, path, workspace string, body io.Reader) (*T, error) {
	respBody, err := c.doAndReadResponse(ctx, method, path, workspace, body)
	if err != nil {
		return nil, err
	}

	var result T
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// doRequestExpectOK executes an HTTP request and returns an error for non 200 responses.
func (c *Client) doRequestExpectOK(ctx context.Context, method, path, workspace string, body io.Reader) error {
	_, err := c.doAndReadResponse(ctx, method, path, workspace, body)
	return err
}

// doAndReadResponse executes an HTTP request with retry logic for transient errors.
// It buffers the request body so it can be replayed on retries.
// Returns the raw response body on success.
func (c *Client) doAndReadResponse(ctx context.Context, method, path, workspace string, body io.Reader) ([]byte, error) {
	// Buffer body for replay on retries.
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("buffering request body: %w", err)
		}
	}

	maxAttempts := c.maxRetries() + 1
	baseDelay := c.retryBaseDelay()
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := backoffDelay(baseDelay, attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		resp, err := c.doRequest(ctx, method, path, workspace, bodyReader)
		if err != nil {
			lastErr = err
			if isRetryableErr(err) && attempt < maxAttempts-1 {
				continue
			}
			return nil, lastErr
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			return respBody, nil
		}

		// Parse the error response.
		var mlErr mlflowError
		if json.Unmarshal(respBody, &mlErr) == nil && mlErr.ErrorCode != "" {
			lastErr = &mlErr
		} else {
			lastErr = fmt.Errorf("mlflow: unexpected status %d: %s", resp.StatusCode, string(respBody))
		}

		if !isRetryableStatus(resp.StatusCode) || attempt >= maxAttempts-1 {
			return nil, lastErr
		}
	}

	return nil, lastErr
}

// isRetryableErr returns true for transient network errors that should be retried.
// Context cancellation and deadline exceeded are never retried since they reflect
// caller intent. Only net.Error (timeouts, connection resets) trigger retries.
func isRetryableErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// isRetryableStatus returns true for HTTP status codes that indicate a transient error.
func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

// backoffDelay returns the wait duration for the given retry attempt using
// exponential backoff with jitter. attempt is 1-based (first retry = 1).
func backoffDelay(base time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		return base
	}
	delay := min(base<<(attempt-1), maxBackoffDelay)
	// Jitter: multiply by a random factor in [0.5, 1.5).
	jitter := 0.5 + rand.Float64() //nolint:gosec
	return time.Duration(float64(delay) * jitter)
}

func (c *Client) doRequest(ctx context.Context, method, path, workspace string, body io.Reader) (*http.Response, error) {
	reqURL := strings.TrimRight(c.BaseURL, "/") + path

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	token, err := c.readToken()
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(WorkspaceHeader, workspace)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient, err := c.httpClient()
	if err != nil {
		return nil, fmt.Errorf("initializing HTTP client: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	return resp, nil
}
