package insales_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
)

func TestGetAccountSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/account.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "app-1" || pass != "pass-1" {
			w.Header().Set("WWW-Authenticate", `Basic realm="InSales"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    42,
			"title": "Test Shop",
		})
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	account, err := client.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if account.ID != 42 {
		t.Errorf("id = %d, want 42", account.ID)
	}
	if account.Title != "Test Shop" {
		t.Errorf("title = %q, want 'Test Shop'", account.Title)
	}
}

func TestGetAccountBasicAuth(t *testing.T) {
	var capturedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 1, "title": "Shop"})
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	client.GetAccount(context.Background())

	expected := base64.StdEncoding.EncodeToString([]byte("app-1:pass-1"))
	if !strings.Contains(capturedAuth, expected) {
		t.Errorf("Authorization header does not contain expected credentials: %s", capturedAuth)
	}
}

func TestGetAccountInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	var resourceErr *insales.ResourceError
	if !strings.Contains(err.Error(), "resource") {
		t.Errorf("expected ResourceError, got: %v", err)
	}
	_ = resourceErr
}

func TestGetAccountMissingID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"title": "No ID"})
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected error for missing account.id")
	}
}

func TestGetAccountAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected API error")
	}
	var apiErr *insales.APIError
	if e, ok := err.(*insales.APIError); ok {
		apiErr = e
		if e.Status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", e.Status)
		}
	} else {
		t.Errorf("expected *APIError, got %T: %v", err, err)
	}
	_ = apiErr
}

func TestGetAccount429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(""))
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected error for 429")
	}
	if apiErr, ok := err.(*insales.APIError); ok {
		if apiErr.Status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", apiErr.Status)
		}
	}
}

func TestGetAccount5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body>Server Error</body></html>"))
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected error for 503")
	}
	if apiErr, ok := err.(*insales.APIError); ok {
		if apiErr.Status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", apiErr.Status)
		}
	}
}

func TestGetAccountTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	hc := &http.Client{Timeout: 100 * time.Millisecond}
	client.SetHTTPClient(hc)

	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if _, ok := err.(*insales.TransportError); !ok {
		t.Errorf("expected TransportError, got %T: %v", err, err)
	}
}

func TestGetAccountLargeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Response bigger than 1MB limit
		large := make([]byte, 2<<20)
		large[0] = '{'
		large[len(large)-1] = '}'
		w.Write(large)
	}))
	defer server.Close()

	client := insales.NewClient(server.URL, "app-1", "pass-1")
	// This should still work (truncated but valid JSON might error)
	_, err := client.GetAccount(context.Background())
	_ = err
	// Just ensure it doesn't crash
}

func TestGetAccountDisallowedRedirect(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":1,"title":"Evil"}`))
	}))
	defer redirectTarget.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer server.Close()

	client := insales.NewStrictClient(server.URL, "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected error for redirect")
	}
}

func TestValidateShopURL(t *testing.T) {
	tests := []struct {
		shop    string
		allowed []string
		want    string
		err     bool
	}{
		{"shop.myinsales.ru", []string{"shop.myinsales.ru"}, "https://shop.myinsales.ru", false},
		{"https://shop.myinsales.ru", []string{"shop.myinsales.ru"}, "https://shop.myinsales.ru", false},
		{"http://Shop.Myinsales.ru", []string{"shop.myinsales.ru"}, "https://shop.myinsales.ru", false},
		{"", []string{"shop.myinsales.ru"}, "", true},
		{"shop.myinsales.ru", nil, "", true},
		{"unknown.com", []string{"shop.myinsales.ru"}, "", true},
		{"sub.shop.myinsales.ru", []string{"shop.myinsales.ru"}, "", true},
		{"shop.myinsales.ru", []string{"myinsales.ru"}, "", true},
	}

	for _, tt := range tests {
		got, err := insales.ValidateShopURL(tt.shop, tt.allowed)
		if tt.err {
			if err == nil {
				t.Errorf("ValidateShopURL(%q) expected error", tt.shop)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateShopURL(%q) unexpected error: %v", tt.shop, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ValidateShopURL(%q) = %q, want %q", tt.shop, got, tt.want)
		}
	}
}

func TestGetAccountConnectionRefused(t *testing.T) {
	client := insales.NewClient("http://127.0.0.1:1", "app-1", "pass-1")
	_, err := client.GetAccount(context.Background())
	if err == nil {
		t.Fatal("expected transport error")
	}
	if _, ok := err.(*insales.TransportError); !ok {
		t.Errorf("expected TransportError, got %T: %v", err, err)
	}
}
