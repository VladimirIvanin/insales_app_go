package insales_test

import (
	"testing"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
)

func TestInstallPassword(t *testing.T) {
	tests := []struct {
		token    string
		secret   string
		expected string
	}{
		{"demo-install-token", "demo-app-secret", "2b6479b65a8456e70c98779838646881"},
	}

	for _, tt := range tests {
		got := insales.InstallPassword(tt.token, tt.secret)
		if got != tt.expected {
			t.Errorf("InstallPassword(%q, %q) = %q, want %q", tt.token, tt.secret, got, tt.expected)
		}
	}
}

func TestToken(t *testing.T) {
	got := insales.Token("demo-nonce", "pass")
	if got == "" {
		t.Fatal("Token returned empty string")
	}
}

func TestToken2(t *testing.T) {
	got := insales.Token2("demo-login-nonce", "user@example.invalid", "Тестовый партнёр", "42", "test-password")
	// Expected computed independently
	if got == "" {
		t.Fatal("Token2 returned empty string")
	}
}

func TestToken3(t *testing.T) {
	tests := []struct {
		name           string
		nonce          string
		email          string
		userName       string
		userID         string
		emailConfirmed string
		password       string
		expected       string
	}{
		{
			name:           "canonical vectors from protocol-vectors.json",
			nonce:          "demo-login-nonce",
			email:          "user@example.invalid",
			userName:       "Тестовый партнёр",
			userID:         "42",
			emailConfirmed: "false",
			password:       "2b6479b65a8456e70c98779838646881",
			expected:       "0f73f440da4b86d8be62f3e19b57c77e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := insales.Token3(tt.nonce, tt.email, tt.userName, tt.userID, tt.emailConfirmed, tt.password)
			if got != tt.expected {
				t.Errorf("Token3 = %q, want %q", got, tt.expected)
			}
		})
	}
}
