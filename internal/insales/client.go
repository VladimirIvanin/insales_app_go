package insales

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const maxResponseBytes = 1 << 20

// Common errors.
type (
	// TransportError represents a network-level failure.
	TransportError struct{ Err error }

	// APIError represents an unexpected HTTP status from the InSales API.
	APIError struct {
		Status     int
		Body       string
		RetryAfter int
	}

	// ResourceError represents a malformed response.
	ResourceError struct{ Err error }
)

func (e *TransportError) Error() string { return "transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("api: status %d: %s", e.Status, body)
}

func (e *ResourceError) Error() string { return "resource: " + e.Err.Error() }
func (e *ResourceError) Unwrap() error { return e.Err }

// IsTemporary reports timeout, network, HTTP 429 and 5xx failures.
func IsTemporary(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	var transportErr *TransportError
	return errors.As(err, &transportErr)
}

// Client is an HTTP client for one InSales shop.
type Client struct {
	baseURL    string
	appID      string
	password   string
	httpClient *http.Client

	mu    sync.Mutex
	usage string
}

// NewClient creates a shop client that does not follow redirects.
func NewClient(baseURL, appID, password string) *Client {
	return &Client{
		baseURL:  stringsTrimRight(baseURL),
		appID:    appID,
		password: password,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// NewStrictClient is the same client used for install checks and authorized calls.
func NewStrictClient(baseURL, appID, password string) *Client {
	return NewClient(baseURL, appID, password)
}

// SetHTTPClient replaces the underlying HTTP client (used in tests).
func (c *Client) SetHTTPClient(hc *http.Client) { c.httpClient = hc }

// UsageLimit returns API-Usage-Limit from the most recent response on this client.
func (c *Client) UsageLimit() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

func (c *Client) setUsage(v string) {
	c.mu.Lock()
	c.usage = v
	c.mu.Unlock()
}

func (c *Client) do(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	req.SetBasicAuth(c.appID, c.password)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	defer resp.Body.Close()
	c.setUsage(resp.Header.Get("API-Usage-Limit"))

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	if len(raw) > maxResponseBytes {
		return nil, &ResourceError{Err: errors.New("response exceeds maximum size")}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		apiErr := &APIError{Status: resp.StatusCode, Body: string(raw)}
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds > 0 {
				apiErr.RetryAfter = seconds
			} else if deadline, parseErr := http.ParseTime(retryAfter); parseErr == nil {
				remaining := time.Until(deadline)
				if remaining > 0 {
					apiErr.RetryAfter = int((remaining + time.Second - 1) / time.Second)
				}
			}
		}
		return nil, apiErr
	}
	return raw, nil
}

func stringsTrimRight(baseURL string) string {
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	return baseURL
}
