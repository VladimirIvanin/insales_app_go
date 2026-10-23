package insales

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryAfterHTTPDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "app", "password").GetAccount(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter < 58 || apiErr.RetryAfter > 60 {
		t.Fatalf("Retry-After HTTP date not preserved: %v", err)
	}
}
