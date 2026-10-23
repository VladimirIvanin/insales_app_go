package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDBStatusAndUpgrade(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	dbPath := filepath.Join(dir, "app.sqlite3")
	t.Setenv("INSALES_APP_ID", "app")
	t.Setenv("INSALES_APP_SECRET", "secret")
	t.Setenv("APP_BASE_URL", "http://127.0.0.1:8765")
	t.Setenv("ALLOWED_SHOPS", "shop.example.test")
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("APP_DB", dbPath)

	if code := run([]string{"db", "status"}); code == 0 {
		t.Fatal("status of a missing database should fail")
	}
	if code := run([]string{"db", "upgrade"}); code != 0 {
		t.Fatal("upgrade failed")
	}
	if code := run([]string{"db", "status"}); code != 0 {
		t.Fatal("status after upgrade failed")
	}
	if code := run([]string{"doctor"}); code != 0 {
		t.Fatal("doctor failed")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := run([]string{"nope"}); code == 0 {
		t.Fatal("unknown command succeeded")
	}
}
