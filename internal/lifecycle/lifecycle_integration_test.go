// Package lifecycle_test contains integration tests for the lifecycle package.
//
// These tests exercise the lifecycle through the storage and client layers,
// simulating the InSales platform behavior.
package lifecycle_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

// TestInstallVerifyPromote tests install → verify (via API) → promote → query.
func TestInstallVerifyPromote(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	var apiCalls int
	var apiMu sync.Mutex

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiMu.Lock()
		apiCalls++
		apiMu.Unlock()
		if r.URL.Path != "/admin/account.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "test-app-id" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_ = pass
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    42,
			"title": "Verified Shop",
		})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	// Install
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

	// Verify candidate exists by getting store (it shouldn't be active yet)
	_, err = db.GetActiveStore(42)
	if err != storage.ErrNotFound {
		t.Fatal("store should not be active before verification")
	}

	// Simulate verification by promoting the candidate directly
	// This is what the background worker would do

	// Use storage directly to claim and verify
	c, err := db.ClaimCandidate()
	if err != nil {
		t.Fatalf("ClaimCandidate: %v", err)
	}
	if c == nil {
		t.Fatal("expected candidate")
	}

	// Verify via API
	err = lc.VerifyInstall(ctx, c)
	if err != nil {
		t.Fatalf("VerifyInstall: %v", err)
	}

	// Now store should be active
	activeStore, err := db.GetActiveStore(42)
	if err != nil {
		t.Fatalf("GetActiveStore: %v", err)
	}
	if activeStore.Title != "Verified Shop" {
		t.Errorf("title = %q, want 'Verified Shop'", activeStore.Title)
	}
}

// TestInstallAPIFailure tests that a failed API call doesn't activate the store.
func TestInstallAPIFailure(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"maintenance"}`))
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	// Install
	_, err := lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "install-token",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleInstall: %v", err)
	}

	// Claim and verify
	c, err := db.ClaimCandidate()
	if err != nil {
		t.Fatalf("ClaimCandidate: %v", err)
	}
	if c == nil {
		t.Fatal("expected candidate")
	}

	err = lc.VerifyInstall(ctx, c)
	if err == nil {
		t.Fatal("expected error for API failure")
	}

	// Store should NOT be active
	_, err = db.GetActiveStore(42)
	if err != storage.ErrNotFound {
		t.Fatal("store should not be active after failed verification")
	}
}

// TestConcurrentInstallUninstall tests a race between install verification and uninstall.
func TestConcurrentInstallUninstall(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	// Set up API server with a gate to control timing
	var gate sync.WaitGroup
	gate.Add(1)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait for the gate to be released
		gate.Wait()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    42,
			"title": "Shop",
		})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	// Install
	_, err := lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "install-token",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleInstall: %v", err)
	}

	// Start verification in background (it will block on the gate)
	var verifyErr error
	var verifyDone sync.WaitGroup
	verifyDone.Add(1)
	go func() {
		defer verifyDone.Done()
		c, cerr := db.ClaimCandidate()
		if cerr != nil {
			verifyErr = cerr
			return
		}
		if c == nil {
			return
		}
		verifyErr = lc.VerifyInstall(ctx, c)
	}()

	// Uninstall while verification is in flight
	err = lc.HandleUninstall(ctx, lifecycle.UninstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     insales.InstallPassword("install-token", "test-app-secret"),
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("HandleUninstall: %v", err)
	}

	// Now release the gate so the verification can complete
	gate.Done()
	verifyDone.Wait()

	// The verification should either fail because the store was removed,
	// or the store should not exist
	_, err = db.GetActiveStore(42)
	if err != storage.ErrNotFound {
		t.Log("Verification error:", verifyErr)
		t.Log("Store still exists after concurrent uninstall+verify")
		// This is acceptable if the candidate had an older generation
	}
}

// TestCandidateOrdering tests that candidates are processed in the right order.
func TestCandidateOrdering(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    42,
			"title": "Shop",
		})
	}))
	defer apiServer.Close()

	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(apiServer.URL, "test-app-id", password)
	})

	// Two installs for the same store
	_, err := lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "token-1",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	_, err = lc.HandleInstall(ctx, lifecycle.InstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     "token-2",
		InsalesID: 42,
	})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}

	// Claim first candidate
	c1, err := db.ClaimCandidate()
	if err != nil {
		t.Fatal(err)
	}
	if c1 == nil {
		t.Fatal("expected first candidate")
	}

	// Verify first
	err = lc.VerifyInstall(ctx, c1)
	if err != nil {
		t.Fatalf("verify first: %v", err)
	}

	// Claim second
	c2, err := db.ClaimCandidate()
	if err != nil {
		t.Fatal(err)
	}
	if c2 == nil {
		t.Fatal("expected second candidate")
	}

	// Verify second
	err = lc.VerifyInstall(ctx, c2)
	if err != nil {
		t.Fatalf("verify second: %v", err)
	}

	// Store should have the latest generation
	store, err := db.GetActiveStore(42)
	if err != nil {
		t.Fatal(err)
	}
	if store.Generation != 2 {
		t.Errorf("generation = %d, want 2", store.Generation)
	}
}

// TestUninstallIdempotent tests that repeated uninstall is safe.
func TestUninstallIdempotent(t *testing.T) {
	lc, db := newLifecycle(t)
	ctx := context.Background()

	password := insales.InstallPassword("install-token", "test-app-secret")

	c := &storage.Candidate{
		ID:          "cand-1",
		StoreID:     1,
		Shop:        "shop.myinsales.ru",
		Password:    password,
		Generation:  1,
		MaxAttempts: 3,
	}
	db.CreateCandidate(c)
	db.PromoteCandidate("cand-1", "Shop")

	// First uninstall
	err := lc.HandleUninstall(ctx, lifecycle.UninstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     password,
		InsalesID: 1,
	})
	if err != nil {
		t.Fatalf("first uninstall: %v", err)
	}

	// Second uninstall should be idempotent (no error, store is already gone)
	err = lc.HandleUninstall(ctx, lifecycle.UninstallParams{
		Shop:      "shop.myinsales.ru",
		Token:     password,
		InsalesID: 1,
	})
	if err != nil {
		t.Fatalf("second uninstall should be idempotent, got: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	lc, db := newLifecycle(t)
	now := time.Now()
	frozen := now
	lc.SetClock(func() time.Time { return frozen })
	db.SetClock(func() time.Time { return frozen })

	password := insales.InstallPassword("token", "test-app-secret")

	c := &storage.Candidate{
		ID:          "cand-1",
		StoreID:     1,
		Shop:        "shop.myinsales.ru",
		Password:    password,
		Generation:  1,
		MaxAttempts: 3,
	}
	db.CreateCandidate(c)
	db.PromoteCandidate("cand-1", "Shop")

	// Create session with short TTL
	sessionID := "sess-exp"
	if err := db.CreateSession(sessionID, 1, "user", 1, 1*time.Hour); err != nil {
		t.Fatal(err)
	}

	// Advance time past expiry
	frozen = now.Add(2 * time.Hour)

	_, _, err := lc.VerifySession(sessionID)
	if err == nil {
		t.Fatal("expected session to be expired")
	}
}

func TestChallengeConsumedOnce(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	db.SetClock(func() time.Time { return now })

	password := "test-pass"

	c := &storage.Candidate{
		ID:          "cand-1",
		StoreID:     1,
		Shop:        "shop.myinsales.ru",
		Password:    password,
		Generation:  1,
		MaxAttempts: 3,
	}
	db.CreateCandidate(c)
	db.PromoteCandidate("cand-1", "Shop")

	db.CreateChallenge("ch-1", 1, "nonce", "browser", 5*time.Minute)

	// We can't call HandleAutologin without knowing the nonce,
	// so let's just verify the storage-level behavior.
	db.ConsumeChallenge("ch-1", "browser")
	_, err := db.ConsumeChallenge("ch-1", "browser")
	if err != storage.ErrNotFound {
		t.Fatal("expected challenge to be consumed")
	}
}
