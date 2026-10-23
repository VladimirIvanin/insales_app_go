package storage_test

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

func newTestDB(t *testing.T) *storage.DB {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(filepath.Join(dir, "test.db"), key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFreshSchema(t *testing.T) {
	db := newTestDB(t)
	applied, err := db.AppliedVersions()
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != len(storage.Migrations) {
		t.Fatalf("versions = %v", applied)
	}
	var name string
	err = db.DB().QueryRow(`SELECT name FROM pragma_table_info('candidates') WHERE name = 'attempts'`).Scan(&name)
	if err != sql.ErrNoRows {
		t.Fatalf("queue column attempts still present: %v %q", err, name)
	}
	if _, err := db.GenerationValue(); err != nil {
		t.Fatal(err)
	}
}

func TestReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	key := bytes32(t)
	db, err := storage.New(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveInstall("c1", 7, "shop.example.test", "secret"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = storage.New(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := db.CountCandidates("shop.example.test")
	if err != nil || n != 1 {
		t.Fatalf("candidates = %d, err %v", n, err)
	}
}

func TestInstallLimitDedupAndTTL(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	db.SetClock(func() time.Time { return now })

	first, err := db.SaveInstall("c1", 1, "shop.example.test", "same")
	if err != nil {
		t.Fatal(err)
	}
	again, err := db.SaveInstall("c2", 99, "shop.example.test", "same")
	if err != nil {
		t.Fatal(err)
	}
	if again.Created {
		t.Fatal("same credential must not create another candidate")
	}
	if again.CandidateID != first.CandidateID || again.Generation != first.Generation {
		t.Fatalf("dedup changed identity: %+v vs %+v", again, first)
	}

	accepted := 0
	for i := 0; i < 6; i++ {
		id := "extra-" + string(rune('a'+i))
		_, err := db.SaveInstall(id, 100+i, "shop.example.test", "pw-"+id)
		if errors.Is(err, storage.ErrCapacity) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		accepted++
	}
	if accepted != storage.MaxCandidatesPerShop-1 {
		t.Fatalf("accepted extras = %d, want %d", accepted, storage.MaxCandidatesPerShop-1)
	}
	kept, err := db.SaveInstall("again", 1, "shop.example.test", "same")
	if err != nil {
		t.Fatal(err)
	}
	if kept.Created || kept.CandidateID != first.CandidateID {
		t.Fatalf("full queue dropped the first credential: %+v", kept)
	}
	n, err := db.CountCandidates("shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if n != storage.MaxCandidatesPerShop {
		t.Fatalf("candidates = %d, want %d", n, storage.MaxCandidatesPerShop)
	}

	oldNow := now
	now = now.Add(8 * 24 * time.Hour)
	if _, err := db.SaveInstall("fresh", 1, "shop.example.test", "brand-new"); err != nil {
		t.Fatal(err)
	}
	n, err = db.CountCandidates("shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after ttl candidates = %d, want 1 (clock %s -> %s)", n, oldNow, now)
	}
}

func TestActiveCredentialDoesNotResetSession(t *testing.T) {
	db := newTestDB(t)
	saved, err := db.SaveInstall("c1", 1, "shop.example.test", "pw")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := db.ActivateCandidate(saved.CandidateID, "Shop")
	if err != nil || !ok {
		t.Fatalf("activate %v %v", ok, err)
	}
	if err := db.CreateSession("sess", 1, "user", saved.Generation, time.Hour); err != nil {
		t.Fatal(err)
	}
	repeat, err := db.SaveInstall("c2", 1, "shop.example.test", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Created {
		t.Fatal("matching active credential created a pending row")
	}
	if _, _, err := db.VerifySession("sess"); err != nil {
		t.Fatalf("session was reset: %v", err)
	}
	n, err := db.CountCandidates("shop.example.test")
	if err != nil || n != 0 {
		t.Fatalf("pending = %d err %v", n, err)
	}
}

func TestLateUninstallKeepsDifferentPending(t *testing.T) {
	db := newTestDB(t)
	active, err := db.SaveInstall("active", 1, "shop.example.test", "active-pw")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(active.CandidateID, "Shop"); err != nil || !ok {
		t.Fatalf("activate %v %v", ok, err)
	}
	if err := db.CreateSession("sess", 1, "user", active.Generation, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateChallenge("ch", "nonce", "browser", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	same, err := db.SaveInstall("same", 1, "shop.example.test", "active-pw")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.SaveInstall("other", 1, "shop.example.test", "other-pw")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := db.Uninstall(1, "shop.example.test", "active-pw")
	if err != nil || !removed {
		t.Fatalf("uninstall %v %v", removed, err)
	}
	if _, err := db.GetActiveStore(1); err != storage.ErrNotFound {
		t.Fatalf("active still present: %v", err)
	}
	var sessions, challenges int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || challenges != 0 {
		t.Fatalf("sessions=%d challenges=%d", sessions, challenges)
	}
	if ok, err := db.ActivateCandidate(same.CandidateID, "Same"); err != nil || ok {
		t.Fatalf("same-password pending survived: activated=%v err=%v", ok, err)
	}
	pending, err := db.NewestPending(1, "shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.ID != other.CandidateID || pending.Password != "other-pw" {
		t.Fatalf("other pending = %+v", pending)
	}
	if ok, err := db.ActivateCandidate(other.CandidateID, "Other"); err != nil || !ok {
		t.Fatalf("activate other %v %v", ok, err)
	}
	store, err := db.GetActiveStore(1)
	if err != nil || store.Password != "other-pw" {
		t.Fatalf("store = %+v err %v", store, err)
	}
}

func TestUninstallWrongPasswordAndIdempotent(t *testing.T) {
	db := newTestDB(t)
	saved, err := db.SaveInstall("c", 1, "shop.example.test", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Uninstall(1, "shop.example.test", "nope"); err != storage.ErrNotAuthorized {
		t.Fatalf("err = %v", err)
	}
	if ok, err := db.ActivateCandidate(saved.CandidateID, "T"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := db.Uninstall(1, "shop.example.test", "nope"); err != storage.ErrNotAuthorized {
		t.Fatalf("active wrong password err = %v", err)
	}
	if _, err := db.GetActiveStore(1); err != nil {
		t.Fatal(err)
	}
	removed, err := db.Uninstall(1, "shop.example.test", "pw")
	if err != nil || !removed {
		t.Fatal(err)
	}
	removed, err = db.Uninstall(1, "shop.example.test", "pw")
	if err != nil || removed {
		t.Fatalf("second uninstall removed=%v err=%v", removed, err)
	}
}

func TestPendingUninstallDoesNotRemoveActive(t *testing.T) {
	db := newTestDB(t)
	active, err := db.SaveInstall("a", 1, "shop.example.test", "active-pw")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(active.CandidateID, "T"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := db.SaveInstall("p", 1, "shop.example.test", "pending-pw"); err != nil {
		t.Fatal(err)
	}
	keep, err := db.SaveInstall("keep", 1, "shop.example.test", "keep-pw")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := db.Uninstall(1, "shop.example.test", "pending-pw")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	store, err := db.GetActiveStore(1)
	if err != nil || store.Password != "active-pw" {
		t.Fatalf("store = %+v err %v", store, err)
	}
	pending, err := db.NewestPending(1, "shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.ID != keep.CandidateID || pending.Password != "keep-pw" {
		t.Fatalf("other pending = %+v", pending)
	}
}

func TestActivateDoesNotRestoreRemovedCandidate(t *testing.T) {
	db := newTestDB(t)
	saved, err := db.SaveInstall("c", 1, "shop.example.test", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCandidate(saved.CandidateID); err != nil {
		t.Fatal(err)
	}
	ok, err := db.ActivateCandidate(saved.CandidateID, "T")
	if err != nil || ok {
		t.Fatalf("activated=%v err=%v", ok, err)
	}
}

func TestStaleCandidateDoesNotReplaceNewerStore(t *testing.T) {
	db := newTestDB(t)
	older, err := db.SaveInstall("old", 1, "shop.example.test", "old-pw")
	if err != nil {
		t.Fatal(err)
	}
	newer, err := db.SaveInstall("new", 1, "shop.example.test", "new-pw")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(newer.CandidateID, "New"); err != nil || !ok {
		t.Fatal(err)
	}
	ok, err := db.ActivateCandidate(older.CandidateID, "Old")
	if err != nil || ok {
		t.Fatalf("stale activate = %v %v", ok, err)
	}
	store, err := db.GetActiveStore(1)
	if err != nil || store.Password != "new-pw" || store.Generation != newer.Generation {
		t.Fatalf("store = %+v err %v", store, err)
	}
}

func TestChallengeAndSessionBoundToGeneration(t *testing.T) {
	db := newTestDB(t)
	saved, err := db.SaveInstall("c", 1, "shop.example.test", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(saved.CandidateID, "T"); err != nil || !ok {
		t.Fatal(err)
	}
	gen, err := db.CreateChallenge("ch", "nonce", "browser", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if gen != saved.Generation {
		t.Fatalf("challenge generation %d", gen)
	}
	if _, err := db.ConsumeChallenge("ch", "other"); err != storage.ErrNotAuthorized {
		t.Fatalf("wrong browser: %v", err)
	}
	ch, err := db.ConsumeChallenge("ch", "browser")
	if err != nil || ch.Nonce != "nonce" {
		t.Fatalf("consume %+v %v", ch, err)
	}
	if _, err := db.ConsumeChallenge("ch", "browser"); err != storage.ErrNotFound {
		t.Fatalf("second consume %v", err)
	}

	if err := db.CreateSession("s", 1, "user", saved.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	next, err := db.SaveInstall("c2", 1, "shop.example.test", "pw2")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(next.CandidateID, "T2"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, err := db.VerifySession("s"); err != storage.ErrNotFound {
		t.Fatalf("old session err = %v", err)
	}
	if err := db.CreateSession("s2", 1, "user", saved.Generation, time.Minute); err != storage.ErrStale {
		t.Fatalf("old generation session err = %v", err)
	}
}

func TestGenerationSurvivesUninstall(t *testing.T) {
	db := newTestDB(t)
	first, err := db.SaveInstall("c", 1, "shop.example.test", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ActivateCandidate(first.CandidateID, "T"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := db.Uninstall(1, "shop.example.test", "pw"); err != nil {
		t.Fatal(err)
	}
	second, err := db.SaveInstall("c2", 1, "shop.example.test", "pw2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("generation reused: %d then %d", first.Generation, second.Generation)
	}
}

func TestUpgradeFromQueueSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	key := bytes32(t)

	freshPath := filepath.Join(dir, "fresh.db")
	fresh, err := storage.New(freshPath, key)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := fresh.SaveInstall("src", 4, "keep.example.test", "migrated-secret")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := fresh.ActivateCandidate(saved.CandidateID, "Kept"); err != nil || !ok {
		t.Fatal(err)
	}
	var cipher string
	if err := fresh.DB().QueryRow(`SELECT password FROM stores WHERE id = 4`).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	fresh.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := `
		CREATE TABLE stores (
			id INTEGER PRIMARY KEY,
			shop TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL DEFAULT 1,
			installation_epoch INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL,
			user_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			installation_epoch INTEGER NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE TABLE challenges (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL,
			nonce TEXT NOT NULL,
			browser_id TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE TABLE candidates (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL,
			shop TEXT NOT NULL,
			password TEXT NOT NULL,
			generation INTEGER NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 3,
			last_error TEXT NOT NULL DEFAULT '',
			lease_token TEXT NOT NULL DEFAULT '',
			next_attempt_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT (datetime('now')));
		INSERT INTO schema_version (version, applied_at) VALUES (1, datetime('now')), (2, datetime('now'));
	`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO stores (id, shop, password, title, generation, installation_epoch, created_at)
		VALUES (4, 'keep.example.test', ?, 'Kept', 3, 9, datetime('now'))`, cipher); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO sessions (id, store_id, user_id, generation, installation_epoch, expires_at, created_at)
		VALUES ('old', 4, 'u', 3, 9, datetime('now', '+1 day'), datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	for i, created := range []string{"datetime('now')", "datetime('now', '-10 days')"} {
		q := `INSERT INTO candidates (id, store_id, shop, password, generation, attempts, max_attempts, last_error, lease_token, next_attempt_at, created_at, updated_at)
			VALUES (?, 4, 'keep.example.test', ?, ?, 3, 3, '', '', datetime('now'), ` + created + `, datetime('now'))`
		if _, err := raw.Exec(q, "cand-"+string(rune('a'+i)), cipher, 4+i); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	upgraded, err := storage.New(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	store, err := upgraded.GetActiveStore(4)
	if err != nil {
		t.Fatal(err)
	}
	if store.Password != "migrated-secret" || store.Title != "Kept" {
		t.Fatalf("store = %+v", store)
	}
	if _, _, err := upgraded.VerifySession("old"); err != storage.ErrNotFound {
		t.Fatalf("old session survived: %v", err)
	}
	n, err := upgraded.CountCandidates("keep.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("candidates after ttl migration = %d", n)
	}
	gen, err := upgraded.GenerationValue()
	if err != nil {
		t.Fatal(err)
	}
	if gen <= 9 {
		t.Fatalf("counter %d is not above saved epoch", gen)
	}
	var attempts string
	err = upgraded.DB().QueryRow(`SELECT name FROM pragma_table_info('candidates') WHERE name = 'lease_token'`).Scan(&attempts)
	if err != sql.ErrNoRows {
		t.Fatalf("lease column present: %v", err)
	}
}

func bytes32(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}
