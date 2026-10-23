package lifecycle_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

func newDB(t *testing.T) *storage.DB {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(filepath.Join(t.TempDir(), "t.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestInstallDoesNotCallAPI(t *testing.T) {
	db := newDB(t)
	var calls atomic.Int32
	lc := lifecycle.NewService(db, "app", "secret")
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		calls.Add(1)
		return insales.NewClient("https://"+shop, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{
		Shop: "shop.example.test", Token: "tok", InsalesID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("api calls = %d", calls.Load())
	}
	if _, err := db.GetActiveStore(7); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("active err = %v", err)
	}
}

func TestFirstLoginVerifiesOnce(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Demo"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{
		Shop: "shop.example.test", Token: "tok", InsalesID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	login, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "browser")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("account calls = %d", calls.Load())
	}
	store, err := db.GetActiveStore(7)
	if err != nil || store.Title != "Demo" {
		t.Fatalf("store %+v %v", store, err)
	}
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "browser"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("repeat login called API: %d", calls.Load())
	}
	_ = login
}

func TestTemporaryErrorKeepsPending(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "tok", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b")
	if !errors.Is(err, lifecycle.ErrTemporary) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := db.CountCandidates("shop.example.test"); n != 1 {
		t.Fatalf("pending = %d", n)
	}
}

func TestTemporaryErrorFallsBackToActive(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	var mode atomic.Value
	mode.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == "fail" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Old"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	install := func(token string) {
		t.Helper()
		if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: token, InsalesID: 7}); err != nil {
			t.Fatal(err)
		}
	}
	install("first")
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b"); err != nil {
		t.Fatal(err)
	}
	mode.Store("fail")
	install("second")
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b"); err != nil {
		t.Fatal(err)
	}
	store, err := db.GetActiveStore(7)
	if err != nil || store.Title != "Old" {
		t.Fatalf("store %+v %v", store, err)
	}
	if n, _ := db.CountCandidates("shop.example.test"); n != 1 {
		t.Fatalf("pending = %d", n)
	}
}

func TestFinalErrorDeletesPending(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "tok", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b")
	if !errors.Is(err, lifecycle.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := db.CountCandidates("shop.example.test"); n != 0 {
		t.Fatalf("pending = %d", n)
	}
}

func TestInvalidJSONKeepsPending(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"title":`))
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "tok", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b")
	if !errors.Is(err, lifecycle.ErrVerification) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := db.CountCandidates("shop.example.test"); n != 1 {
		t.Fatalf("pending = %d", n)
	}
}

func TestIDMismatchDeletesPendingAndKeepsActive(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	var id atomic.Int32
	id.Store(7)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id.Load(), "title": "X"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "first", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b"); err != nil {
		t.Fatal(err)
	}
	id.Store(8)
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "second", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b"); err != nil {
		t.Fatal(err)
	}
	store, err := db.GetActiveStore(7)
	if err != nil || store.Password != insales.InstallPassword("first", "secret") {
		t.Fatalf("store password changed: %+v %v", store, err)
	}
	if n, _ := db.CountCandidates("shop.example.test"); n != 0 {
		t.Fatalf("pending = %d", n)
	}
}

func TestForeignShopRejectedBeforeAPI(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "X"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "a.example.test", Token: "tok", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "a.example.test", InsalesID: 7}, "b"); err != nil {
		t.Fatal(err)
	}
	_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "b.example.test", InsalesID: 7}, "b")
	if !errors.Is(err, lifecycle.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("api calls = %d", calls.Load())
	}
}

func TestUninstallDuringVerification(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Late"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	res, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "tok", InsalesID: 7})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b")
		errCh <- err
	}()
	<-entered
	if err := lc.HandleUninstall(context.Background(), lifecycle.UninstallParams{
		Shop: "shop.example.test", Token: res.Password, InsalesID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errCh; !errors.Is(err, lifecycle.ErrStoreNotInstalled) {
		t.Fatalf("login err = %v", err)
	}
	if _, err := db.GetActiveStore(7); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("store restored: %v", err)
	}
}

func TestParallelLoginsActivateOnce(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Once"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "tok", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "b")
			errs <- err
		}()
	}
	<-started
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	store, err := db.GetActiveStore(7)
	if err != nil || store.Title != "Once" {
		t.Fatalf("%+v %v", store, err)
	}
}

func TestOldChallengeDoesNotReceiveNewInstall(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Shop"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	first, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "one", InsalesID: 7})
	if err != nil {
		t.Fatal(err)
	}
	login, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "browser")
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.GetActiveStore(7)
	if err != nil {
		t.Fatal(err)
	}
	token3 := insales.Token3(login.Nonce, "a@b.c", "N", "1", "true", store.Password)
	if err := lc.HandleUninstall(context.Background(), lifecycle.UninstallParams{Shop: "shop.example.test", Token: first.Password, InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "two", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "browser"); err != nil {
		t.Fatal(err)
	}
	_, err = lc.HandleAutologin(context.Background(), lifecycle.AutologinParams{
		ChallengeID: login.ChallengeID, Token3: token3, UserEmail: "a@b.c", UserName: "N", UserID: "1", EmailConfirmed: "true", BrowserID: "browser",
	})
	if !errors.Is(err, lifecycle.ErrExpired) && !errors.Is(err, lifecycle.ErrForbidden) {
		t.Fatalf("old challenge err = %v", err)
	}
	if _, _, err := db.VerifySession("nope"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestTwoStoresIsolated(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		id := 1
		if pass == insales.InstallPassword("b", "secret") {
			id = 2
		}
		_ = user
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "S"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	for _, item := range []struct {
		id    int
		shop  string
		token string
	}{{1, "a.example.test", "a"}, {2, "b.example.test", "b"}} {
		if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: item.shop, Token: item.token, InsalesID: item.id}); err != nil {
			t.Fatal(err)
		}
		if _, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: item.shop, InsalesID: item.id}, "b"); err != nil {
			t.Fatal(err)
		}
	}
	if err := lc.HandleUninstall(context.Background(), lifecycle.UninstallParams{Shop: "a.example.test", Token: insales.InstallPassword("a", "secret"), InsalesID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetActiveStore(2); err != nil {
		t.Fatal(err)
	}
}

func TestToken3Session(t *testing.T) {
	db := newDB(t)
	lc := lifecycle.NewService(db, "app", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": "Shop"})
	}))
	defer srv.Close()
	lc.SetClientFactory(func(shop, password string) *insales.Client {
		return insales.NewClient(srv.URL, "app", password)
	})
	if _, err := lc.HandleInstall(context.Background(), lifecycle.InstallParams{Shop: "shop.example.test", Token: "tok", InsalesID: 7}); err != nil {
		t.Fatal(err)
	}
	login, err := lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "browser")
	if err != nil {
		t.Fatal(err)
	}
	store, _ := db.GetActiveStore(7)
	bad := insales.Token3(login.Nonce, "a@b.c", "N", "1", "true", "wrong")
	if _, err := lc.HandleAutologin(context.Background(), lifecycle.AutologinParams{
		ChallengeID: login.ChallengeID, Token3: bad, UserEmail: "a@b.c", UserName: "N", UserID: "1", EmailConfirmed: "true", BrowserID: "browser",
	}); !errors.Is(err, lifecycle.ErrInvalidSignature) {
		t.Fatalf("bad token3: %v", err)
	}
	login, err = lc.HandleLogin(context.Background(), lifecycle.LoginParams{Shop: "shop.example.test", InsalesID: 7}, "browser")
	if err != nil {
		t.Fatal(err)
	}
	good := insales.Token3(login.Nonce, "a@b.c", "N", "1", "true", store.Password)
	res, err := lc.HandleAutologin(context.Background(), lifecycle.AutologinParams{
		ChallengeID: login.ChallengeID, Token3: good, UserEmail: "a@b.c", UserName: "N", UserID: "1", EmailConfirmed: "true", BrowserID: "browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lc.VerifySession(res.SessionID); err != nil {
		t.Fatal(err)
	}
}
