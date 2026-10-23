package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ivaninvladimir/insales_app_go/internal/config"
)

func TestLoadRequiresAllowlistAndKeepsExistingEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(`
INSALES_APP_ID=from-file
INSALES_APP_SECRET=file-secret
APP_BASE_URL=https://app.example.test
ALLOWED_SHOPS=https://Shop.Example.test, shop.example.test
ENCRYPTION_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INSALES_APP_ID", "from-env")
	t.Setenv("INSALES_APP_SECRET", "")
	os.Unsetenv("INSALES_APP_SECRET")
	os.Unsetenv("APP_BASE_URL")
	os.Unsetenv("ALLOWED_SHOPS")
	os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != "from-env" {
		t.Fatalf("app id %q", cfg.AppID)
	}
	if cfg.AppSecret != "file-secret" {
		t.Fatalf("secret was not loaded from .env")
	}
	if len(cfg.AllowedShops) != 1 || cfg.AllowedShops[0] != "shop.example.test" {
		t.Fatalf("allowlist %#v", cfg.AllowedShops)
	}
}

func TestLoadRejectsEmptyAllowlist(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("INSALES_APP_ID", "app")
	t.Setenv("INSALES_APP_SECRET", "secret")
	t.Setenv("APP_BASE_URL", "https://app.example.test")
	t.Setenv("ALLOWED_SHOPS", " , ")
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if _, err := config.Load(); err == nil {
		t.Fatal("empty allowlist was accepted")
	}
}
