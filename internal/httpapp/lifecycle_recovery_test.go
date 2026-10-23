package httpapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/config"
	"github.com/ivaninvladimir/insales_app_go/internal/httpapp"
	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

func recoveryDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "recovery.sqlite3"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func recoveryActive(t *testing.T, db *storage.DB, id int, shop, password string) int64 {
	t.Helper()
	saved, err := db.SaveInstall(fmt.Sprintf("candidate-%d", id), id, shop, password)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(saved.CandidateID, shop); err != nil || !ok {
		t.Fatalf("activate: %v %v", ok, err)
	}
	return saved.Generation
}

func recoveryHTTP(t *testing.T, db *storage.DB, lc *lifecycle.Service, allowed ...string) *httptest.Server {
	t.Helper()
	app, err := httpapp.New(&config.Config{AppID: "app", AppSecret: "secret", BaseURL: "http://localhost", AllowedShops: allowed}, lc, db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func recoveryClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestRecoveryDomainChangeWithAcceptedInstall(t *testing.T) {
	db := recoveryDB(t)
	recoveryActive(t, db, 7, "old.example.test", "old-password")
	lc := lifecycle.NewService(db, "app", "secret")
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Moved"})
	}))
	defer api.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client { return insales.NewClient(api.URL, "app", password) })
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "new.example.test", Token: "new", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "new.example.test", InsalesID: 7}, "browser")
	if err != nil {
		t.Fatalf("accepted domain change rejected before verification: %v; API calls=%d", err, calls.Load())
	}
}

func TestRecoveryPendingLoginBudget(t *testing.T) {
	db := recoveryDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "real", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	db.SetClock(func() time.Time { return now })
	var recovered atomic.Bool
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if recovered.Load() {
			_, _ = io.WriteString(w, `{"id":7,"title":"Recovered"}`)
			return
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer api.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client { return insales.NewClient(api.URL, "app", password) })
	srv := recoveryHTTP(t, db, lc, "shop.example.test")
	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/login?shop=shop.example.test&insales_id=7")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Fatalf("status=%d", resp.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("Retry-After=60 ignored across public logins: API calls=%d", calls.Load())
	}
	now = now.Add(61 * time.Second)
	recovered.Store(true)
	resp, err := recoveryClient(t).Get(srv.URL + "/login?shop=shop.example.test&insales_id=7")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || calls.Load() != 2 {
		t.Fatalf("recovery status=%d API calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestRecoveryForgedPrefillAdmission(t *testing.T) {
	db := recoveryDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, _ := r.BasicAuth()
		if password != insales.InstallPassword("real-token", "secret") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Actual"})
	}))
	defer api.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client { return insales.NewClient(api.URL, "app", password) })
	srv := recoveryHTTP(t, db, lc, "shop.example.test")
	for i := 0; i < storage.MaxCandidatesPerShop; i++ {
		resp, err := http.Get(fmt.Sprintf("%s/install?shop=shop.example.test&token=forged-%d&insales_id=%d", srv.URL, i, 1000+i))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("prefill status=%d", resp.StatusCode)
		}
	}
	resp, err := http.Get(srv.URL + "/install?shop=shop.example.test&token=real-token&insales_id=7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("legitimate install rejected after five unauthenticated forged callbacks: status=%d", resp.StatusCode)
	}
}

func TestRecoveryOverlappingBrowserLogins(t *testing.T) {
	db := recoveryDB(t)
	recoveryActive(t, db, 1, "a.example.test", "password-a")
	recoveryActive(t, db, 2, "b.example.test", "password-b")
	lc := lifecycle.NewService(db, "app", "secret")
	srv := recoveryHTTP(t, db, lc, "a.example.test", "b.example.test")
	client := recoveryClient(t)
	var nonces []string
	var flows []string
	for i, shop := range []string{"a.example.test", "b.example.test"} {
		resp, err := client.Get(fmt.Sprintf("%s/login?shop=%s&insales_id=%d", srv.URL, shop, i+1))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 302 {
			t.Fatalf("login status=%d", resp.StatusCode)
		}
		redirect, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		nonces = append(nonces, redirect.Query().Get("token"))
		callback, parseErr := url.Parse(redirect.Query().Get("login"))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		flows = append(flows, callback.Query().Get("flow"))
	}
	var statuses []int
	for i, password := range []string{"password-a", "password-b"} {
		q := url.Values{"user_email": {"user@example.test"}, "user_name": {"User"}, "user_id": {"7"}, "email_confirmed": {"true"}}
		q.Set("flow", flows[i])
		q.Set("token3", insales.Token3(nonces[i], "user@example.test", "User", "7", "true", password))
		resp, err := client.Get(srv.URL + "/auth/callback?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}
	if statuses[0] != 302 || statuses[1] != 302 {
		t.Fatalf("valid callbacks A/B share and destroy one challenge cookie: statuses=%v", statuses)
	}
}

func TestRecoveryStaleTabUsesItsShop(t *testing.T) {
	db := recoveryDB(t)
	genA := recoveryActive(t, db, 1, "a.example.test", "password-a")
	genB := recoveryActive(t, db, 2, "b.example.test", "password-b")
	if err := db.CreateSession("session-a", 1, "user", genA, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession("session-b", 2, "user", genB, time.Hour); err != nil {
		t.Fatal(err)
	}
	lc := lifecycle.NewService(db, "app", "secret")
	calledShops := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 2, "title": "B"})
	}))
	defer api.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		calledShops <- shop
		return insales.NewClient(api.URL, "app", password)
	})
	srv := recoveryHTTP(t, db, lc, "a.example.test", "b.example.test")
	client := recoveryClient(t)
	origin, _ := url.Parse(srv.URL)
	client.Jar.SetCookies(origin, []*http.Cookie{{Name: "session_id", Value: "session-a", Path: "/"}})
	pageA, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, readErr := io.ReadAll(pageA.Body)
	_ = pageA.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(html), `action="/probe?store_id=1"`) {
		t.Fatalf("tab context missing from form")
	}
	if pageA.StatusCode != 200 {
		t.Fatalf("page A status=%d", pageA.StatusCode)
	}
	client.Jar.SetCookies(origin, []*http.Cookie{{Name: "session_id", Value: "session-b", Path: "/"}})
	probe, err := client.Post(srv.URL+"/probe?store_id=1", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusConflict {
		t.Fatalf("stale probe status=%d, want409", probe.StatusCode)
	}
	select {
	case shop := <-calledShops:
		t.Fatalf("stale tab called API for %q", shop)
	default:
	}
}
