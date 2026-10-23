package insales

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Common errors.
type (
	// TransportError represents a network-level failure (timeout, DNS, connection refused, etc.).
	TransportError struct{ Err error }

	// APIError represents an unexpected HTTP status from the InSales API.
	APIError struct {
		Status     int
		Body       string
		RetryAfter int // seconds, from Retry-After header
	}

	// ResourceError represents a malformed response (invalid JSON, wrong shape).
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

// Account represents a minimal InSales shop account.
type Account struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// Client is an HTTP client for the InSales API bound to a single shop.
type Client struct {
	baseURL    string
	appID      string
	password   string
	httpClient *http.Client
}

// NewClient creates a new InSales API client.
// baseURL must be the shop URL (e.g., "https://shop.myinsales.ru").
// appID is the application public ID.
// password is the store-specific install password.
func NewClient(baseURL, appID, password string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		appID:      appID,
		password:   password,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SetHTTPClient replaces the underlying HTTP client (used in tests).
func (c *Client) SetHTTPClient(hc *http.Client) { c.httpClient = hc }

// GetAccount retrieves the shop account information.
// GET /admin/account.json
func (c *Client) GetAccount(ctx context.Context) (*Account, error) {
	u := c.baseURL + "/admin/account.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &TransportError{err}
	}
	req.SetBasicAuth(c.appID, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &TransportError{err}
	}
	defer resp.Body.Close()

	// Limit response body to 1MB.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &TransportError{err}
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{Status: resp.StatusCode, Body: string(body)}
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			if seconds, err := fmt.Sscanf(retryAfter, "%d", &apiErr.RetryAfter); err == nil && seconds == 1 {
				// parsed ok
			}
		}
		return nil, apiErr
	}

	var account Account
	if err := json.Unmarshal(body, &account); err != nil {
		return nil, &ResourceError{fmt.Errorf("invalid JSON: %w", err)}
	}
	if account.ID == 0 {
		return nil, &ResourceError{fmt.Errorf("missing account.id in response: %s", string(body))}
	}

	return &account, nil
}

// ValidateShopURL checks that the shop URL is valid and allowed.
// Returns the normalized HTTPS URL or an error.
// Only trusts the parsed hostname — rejects userinfo, path, fragment, and empty host.
// An empty allowed list means any valid InSales shop domain is permitted.
func ValidateShopURL(shop string, allowed []string) (string, error) {
	if shop == "" {
		return "", fmt.Errorf("shop is empty")
	}

	// Add scheme if missing for proper parsing
	input := shop
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}

	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("invalid shop URL: %w", err)
	}

	// Reject userinfo, path, fragment, query
	if u.User != nil {
		return "", fmt.Errorf("shop URL contains userinfo")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("shop URL contains path")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("shop URL contains fragment")
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("shop URL contains query")
	}

	hostname := u.Hostname()
	if hostname == "" {
		return "", fmt.Errorf("invalid shop URL: empty host")
	}

	// Reject IP addresses (must be a domain)
	if net.ParseIP(hostname) != nil {
		return "", fmt.Errorf("shop URL is an IP address, domain required")
	}

	// Must have at least one dot (domain)
	if !strings.Contains(hostname, ".") {
		return "", fmt.Errorf("shop %q is not a valid domain", hostname)
	}

	// Check allowed shops list
	if len(allowed) > 0 {
		matched := false
		for _, a := range allowed {
			if hostname == a || strings.HasSuffix(hostname, "."+a) {
				matched = true
				break
			}
		}
		if !matched {
			return "", fmt.Errorf("shop %q is not in the allowed list", hostname)
		}
	}

	return "https://" + hostname, nil
}

// IsTransientError returns true if the error is transient and the operation should be retried.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if asError(err, &apiErr) {
		return apiErr.Status >= 429 || apiErr.Status >= 500
	}
	var transportErr *TransportError
	if asError(err, &transportErr) {
		return true
	}
	return false
}

// asError is a generic errors.As helper for Go 1.23 compatibility.
func asError[T error](err error, target *T) bool {
	for {
		if e, ok := err.(T); ok {
			*target = e
			return true
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
		} else {
			return false
		}
	}
}

// ensureNoRedirect is a CheckRedirect policy that rejects all redirects.
func ensureNoRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// NewStrictClient creates a client with redirects disabled and strict timeouts.
func NewStrictClient(baseURL, appID, password string) *Client {
	c := NewClient(baseURL, appID, password)
	c.httpClient = &http.Client{
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
		CheckRedirect: ensureNoRedirect,
	}
	return c
}
