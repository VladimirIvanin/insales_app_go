package lifecycle_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

func newTestDB(t *testing.T) *storage.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(dbPath, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newLifecycle(t *testing.T) (*lifecycle.Service, *storage.DB) {
	t.Helper()
	db := newTestDB(t)
	lc := lifecycle.NewService(db, "test-app-id", "test-app-secret")
	now := time.Now()
	lc.SetClock(func() time.Time { return now })
	return lc, db
}

// TestInstallAndVerify tests the full install flow with API verification.
func TestInstallAndVerify(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    42,
			"title": "Test Shop",
		})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	result, err := lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "install-token",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleInstall: %v", err)
	}
	if result.Password == "" {
		t.Fatal("password should not be empty")
	}
	if result.CandidateID == "" {
		t.Fatal("candidate ID should not be empty")
	}

	// Claim candidate via storage directly
	candidate, err := db.ClaimCandidate()
	if err != nil {
		t.Fatalf("ClaimCandidate: %v", err)
	}
	if candidate == nil {
		t.Fatal("expected a candidate")
	}

	err = lc.VerifyInstall(ctx, candidate)
	if err != nil {
		t.Fatalf("VerifyInstall: %v", err)
	}
}

// TestInstallMismatchedID verifies that account ID mismatch rejects installation.
func TestInstallMismatchedID(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    99,
			"title": "Wrong Shop",
		})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	_, err := lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "install-token",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleInstall: %v", err)
	}

	candidate, err := db.ClaimCandidate()
	if err != nil {
		t.Fatalf("ClaimCandidate: %v", err)
	}
	if candidate == nil {
		t.Fatal("expected candidate")
	}

	err = lc.VerifyInstall(ctx, candidate)
	if err == nil {
		t.Fatal("expected error for mismatched account ID")
	}
	if !strings.Contains(err.Error(), "account ID mismatch") {
		t.Errorf("expected account ID mismatch error, got: %v", err)
	}
}

// TestFullLifecycle tests install → verify → login → logout → uninstall.
func TestFullLifecycle(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    42,
			"title": "Full Lifecycle Shop",
		})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	// 1. Install
	_, err := lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "install-token",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleInstall: %v", err)
	}

	// 2. Verify
	candidate, err := db.ClaimCandidate()
	if err != nil {
		t.Fatalf("ClaimCandidate: %v", err)
	}
	if candidate == nil {
		t.Fatal("expected candidate")
	}
	if err := lc.VerifyInstall(ctx, candidate); err != nil {
		t.Fatalf("VerifyInstall: %v", err)
	}

	// 3. Verify store is active
	store, err := db.GetActiveStore(42)
	if err != nil {
		t.Fatalf("GetActiveStore: %v", err)
	}
	if store.Shop != "shop.myinsales.ru" {
		t.Errorf("store shop = %q", store.Shop)
	}

	// 4. Logout test — create session and verify
	sessionID := "test-session-id"
	if err := db.CreateSession(sessionID, 42, "user-1", store.Generation, 1*time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := lc.Logout(sessionID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// Session should be gone
	_, _, err = lc.VerifySession(sessionID)
	if err == nil {
		t.Fatal("expected session to be invalid after logout")
	}

	// 5. Uninstall
	err = lc.HandleUninstall(ctx, lifecycle.UninstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     store.Password,
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleUninstall: %v", err)
	}

	// Verify store is gone
	_, err = db.GetActiveStore(42)
	if err != storage.ErrNotFound {
		t.Fatal("expected store to be removed after uninstall")
	}
}

// TestSessionGenerationCheck verifies sessions are invalidated when store is reinstalled.
func TestSessionGenerationCheck(t *testing.T) {
	lc, db := newLifecycle(t)
	now := time.Now()

	// Set up store with gen 1
	c := &storage.Candidate{
		ID:          "cand-1",
		StoreID:     1,
		Shop:        "shop.myinsales.ru",
		Password:    "pass",
		Generation:  1,
		MaxAttempts: 3,
	}
	db.CreateCandidate(c)
	db.PromoteCandidate("cand-1", "Shop")
	db.CreateSession("sess-1", 1, "user-1", 1, 1*time.Hour)

	// Verify works with gen 1
	lc.SetClock(func() time.Time { return now })
	sess, store, err := lc.VerifySession("sess-1")
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if sess.Generation != 1 || store.Generation != 1 {
		t.Fatal("generation mismatch in initial state")
	}

	// Reinstall with new generation
	c2 := &storage.Candidate{
		ID:          "cand-2",
		StoreID:     1,
		Shop:        "shop.myinsales.ru",
		Password:    "new-pass",
		Generation:  2,
		MaxAttempts: 3,
	}
	db.CreateCandidate(c2)
	db.PromoteCandidate("cand-2", "Reinstalled Shop")

	// Old session should be invalid now
	_, _, err = lc.VerifySession("sess-1")
	if err == nil {
		t.Fatal("expected session to be invalid after reinstall")
	}
}

// TestTwoStoresIsolation tests that two stores don't interfere.
func TestTwoStoresIsolation(t *testing.T) {
	lc, db := newLifecycle(t)
	now := time.Now()
	lc.SetClock(func() time.Time { return now })
	db.SetClock(func() time.Time { return now })

	// Set up store 1
	db.CreateCandidate(&storage.Candidate{ID: "cand-1", StoreID: 1, Shop: "shop1.myinsales.ru", Password: "pass-1", Generation: 1, MaxAttempts: 3})
	db.PromoteCandidate("cand-1", "Shop One")

	// Set up store 2
	db.CreateCandidate(&storage.Candidate{ID: "cand-2", StoreID: 2, Shop: "shop2.myinsales.ru", Password: "pass-2", Generation: 1, MaxAttempts: 3})
	db.PromoteCandidate("cand-2", "Shop Two")

	// Create sessions for each store
	db.CreateSession("sess-store1", 1, "user-1", 1, 1*time.Hour)
	db.CreateSession("sess-store2", 2, "user-2", 1, 1*time.Hour)

	// Verify each session
	_, store1, err := lc.VerifySession("sess-store1")
	if err != nil {
		t.Fatalf("VerifySession store 1: %v", err)
	}
	if store1.ID != 1 {
		t.Errorf("store id = %d, want 1", store1.ID)
	}

	_, store2, err := lc.VerifySession("sess-store2")
	if err != nil {
		t.Fatalf("VerifySession store 2: %v", err)
	}
	if store2.ID != 2 {
		t.Errorf("store id = %d, want 2", store2.ID)
	}

	// Uninstall store 1
	err = lc.HandleUninstall(context.Background(), lifecycle.UninstallParams{
		Shop: "shop1.myinsales.ru", Token: "pass-1", InsalesID: 1,
	})
	if err != nil {
		t.Fatalf("HandleUninstall: %v", err)
	}

	// Store 1 session should be invalid
	_, _, err = lc.VerifySession("sess-store1")
	if err == nil {
		t.Fatal("expected store 1 session to be invalid after uninstall")
	}
}

// TestForbiddenUninstall verifies wrong password is rejected.
func TestForbiddenUninstall(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	db.CreateCandidate(&storage.Candidate{ID: "cand-1", StoreID: 1, Shop: "shop.myinsales.ru", Password: "correct-password", Generation: 1, MaxAttempts: 3})
	db.PromoteCandidate("cand-1", "Shop")

	err := lc.HandleUninstall(ctx, lifecycle.UninstallParams{
		Shop: "shop.myinsales.ru", Token: "wrong-password", InsalesID: 1,
	})
	if err != lifecycle.ErrForbidden {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

// TestExpiredChallenge verifies expired challenges are rejected.
func TestExpiredChallenge(t *testing.T) {
	lc, db := newLifecycle(t)
	now := time.Now()
	lc.SetClock(func() time.Time { return now })
	db.SetClock(func() time.Time { return now })

	db.CreateCandidate(&storage.Candidate{ID: "cand-1", StoreID: 1, Shop: "shop.myinsales.ru", Password: "pass", Generation: 1, MaxAttempts: 3})
	db.PromoteCandidate("cand-1", "Shop")

	db.CreateChallenge("ch-1", 1, "nonce", "browser", -1*time.Minute)

	_, err := lc.HandleAutologin(context.Background(), lifecycle.AutologinParams{
		ChallengeID: "ch-1",
		BrowserID:   "browser",
	})
	if err != lifecycle.ErrExpired {
		t.Fatalf("expected ErrExpired, got: %v", err)
	}
}

// TestReinstall tests that a store can be reinstalled.
func TestReinstall(t *testing.T) {
	lc, _ := newLifecycle(t)
	ctx := context.Background()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 42, "title": "Reinstalled Shop"})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	// First install
	_, err := lc.HandleInstall(ctx, lifecycle.InstallParams{Shop: "shop.myinsales.ru", Token: "token-1", InsalesID: 42})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Second install (reinstall)
	result2, err := lc.HandleInstall(ctx, lifecycle.InstallParams{Shop: "shop.myinsales.ru", Token: "token-2", InsalesID: 42})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	_ = result2
}

func TestProtocolVectors(t *testing.T) {
	expectedPassword := "2b6479b65a8456e70c98779838646881"
	got := insales.InstallPassword("demo-install-token", "demo-app-secret")
	if got != expectedPassword {
		t.Errorf("InstallPassword = %q, want %q", got, expectedPassword)
	}

	expectedToken3 := "0f73f440da4b86d8be62f3e19b57c77e"
	got3 := insales.Token3("demo-login-nonce", "user@example.invalid", "Тестовый партнёр", "42", "false", expectedPassword)
	if got3 != expectedToken3 {
		t.Errorf("Token3 = %q, want %q", got3, expectedToken3)
	}
}

func BenchmarkInstallPassword(b *testing.B) {
	for i := 0; i < b.N; i++ {
		insales.InstallPassword("token", "secret")
	}
}

// TestLoginWithToken3 tests login challenge creation works.
func TestLoginWithToken3(t *testing.T) {
	lc, db := newLifecycle(t)

	db.CreateCandidate(&storage.Candidate{ID: "cand-1", StoreID: 1, Shop: "shop.myinsales.ru", Password: "test-store-password", Generation: 1, MaxAttempts: 3})
	db.PromoteCandidate("cand-1", "My Shop")

	now := time.Now()
	lc.SetClock(func() time.Time { return now })
	db.SetClock(func() time.Time { return now })

	_, err := lc.HandleLogin(lifecycle.LoginParams{Shop: "shop.myinsales.ru", InsalesID: 1, UserID: "user-42"}, "test-browser")
	if err != nil {
		t.Fatalf("HandleLogin: %v", err)
	}
}
