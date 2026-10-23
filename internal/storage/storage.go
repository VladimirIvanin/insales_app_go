// Package storage provides SQLite-backed persistence for InSales app state.
//
// Один процесс и один файл. Подключение, кандидат установки, challenge и
// сессия связаны общим номером поколения. Фоновой очереди нет.
package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// MaxCandidatesPerShop — сколько непроверенных установок одного домена
	// хранить одновременно. Новый пароль сверх лимита отклоняется и не удаляет
	// уже принятые записи. Место освобождает CandidateTTL.
	MaxCandidatesPerShop = 5
	// CandidateTTL — срок жизни непроверенного кандидата.
	CandidateTTL = 7 * 24 * time.Hour
)

// Common errors.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrNotAuthorized = errors.New("not authorized")
	ErrStale         = errors.New("stale generation")
	ErrCapacity      = errors.New("too many pending installs")
)

// Migration describes one schema version.
type Migration struct {
	Version int
	Name    string
}

// Migrations is the ordered list of schema versions.
var Migrations = []Migration{
	{Version: 1, Name: "начальная схема"},
	{Version: 2, Name: "installation_epoch"},
	{Version: 3, Name: "проверка при входе и один счётчик поколений"},
}

// Store is a verified shop connection.
type Store struct {
	ID         int
	Shop       string
	Password   string
	Title      string
	Generation int64
	CreatedAt  time.Time
}

// Session is an authenticated browser session bound to an installation generation.
type Session struct {
	ID         string
	StoreID    int
	UserID     string
	Generation int64
	ExpiresAt  time.Time
}

// Challenge is a one-time login nonce bound to a browser and installation generation.
type Challenge struct {
	ID         string
	StoreID    int
	Nonce      string
	BrowserID  string
	Generation int64
	ExpiresAt  time.Time
}

// Candidate is an install that has not been verified yet.
type Candidate struct {
	ID         string
	StoreID    int
	Shop       string
	Password   string
	Generation int64
	CreatedAt  time.Time
}

// InstallSave is the result of accepting an install callback.
type InstallSave struct {
	CandidateID string
	Generation  int64
	// Created is false when the same credential is already active or pending.
	Created bool
}

// DB wraps the SQLite database connection and provides storage operations.
type DB struct {
	db     *sql.DB
	aesGCM cipher.AEAD
	now    func() time.Time
}

// Open opens a SQLite database without applying migrations.
func Open(dbPath string, encryptionKey []byte) (*DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("new GCM: %w", err)
	}

	db.SetMaxOpenConns(1)
	return &DB{db: db, aesGCM: aesGCM, now: time.Now}, nil
}

// New opens a database and applies migrations.
func New(dbPath string, encryptionKey []byte) (*DB, error) {
	s, err := Open(dbPath, encryptionKey)
	if err != nil {
		return nil, err
	}
	if err := s.Upgrade(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// SetClock replaces the time source (used in tests).
func (s *DB) SetClock(now func() time.Time) { s.now = now }

// DB returns the underlying *sql.DB.
func (s *DB) DB() *sql.DB { return s.db }

// Close closes the database connection.
func (s *DB) Close() error { return s.db.Close() }

// Upgrade applies any missing migrations.
func (s *DB) Upgrade() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	steps := []struct {
		version int
		apply   func() error
	}{
		{1, s.migrateV1},
		{2, s.migrateV2},
		{3, s.migrateV3},
	}
	for _, step := range steps {
		applied, err := s.versionApplied(step.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := step.apply(); err != nil {
			return fmt.Errorf("apply migration %d: %w", step.version, err)
		}
	}
	return nil
}

func (s *DB) versionApplied(version int) (bool, error) {
	var exists int
	err := s.db.QueryRow("SELECT COUNT(*) FROM schema_version WHERE version = ?", version).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check migration %d: %w", version, err)
	}
	return exists > 0, nil
}

func (s *DB) recordVersion(tx *sql.Tx, version int) error {
	_, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", version)
	return err
}

func (s *DB) migrateV1() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS stores (
			id INTEGER PRIMARY KEY,
			shop TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS challenges (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
			nonce TEXT NOT NULL,
			browser_id TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS candidates (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL,
			shop TEXT NOT NULL,
			password TEXT NOT NULL,
			generation INTEGER NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 3,
			last_error TEXT NOT NULL DEFAULT '',
			lease_token TEXT NOT NULL DEFAULT '',
			next_attempt_at TEXT NOT NULL DEFAULT (datetime('now')),
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_candidates_claim ON candidates(attempts, next_attempt_at);
		CREATE INDEX IF NOT EXISTS idx_candidates_store ON candidates(store_id);
		CREATE INDEX IF NOT EXISTS idx_sessions_store ON sessions(store_id);
		CREATE INDEX IF NOT EXISTS idx_challenges_store ON challenges(store_id);
	`); err != nil {
		return err
	}
	if err := s.recordVersion(tx, 1); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DB) migrateV2() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		ALTER TABLE stores ADD COLUMN installation_epoch INTEGER NOT NULL DEFAULT 1;
		ALTER TABLE sessions ADD COLUMN installation_epoch INTEGER NOT NULL DEFAULT 1;
		CREATE INDEX IF NOT EXISTS idx_stores_epoch ON stores(installation_epoch);
	`); err != nil {
		return err
	}
	if err := s.recordVersion(tx, 2); err != nil {
		return err
	}
	return tx.Commit()
}

func closeRows(rows *sql.Rows, err error) error {
	return errors.Join(err, rows.Close())
}

func (s *DB) migrateV3() error {
	if _, err := s.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = s.db.Exec(`PRAGMA foreign_keys = ON`) }()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var maxGen sql.NullInt64
	if err := tx.QueryRow(`
		SELECT MAX(v) FROM (
			SELECT generation AS v FROM stores
			UNION ALL SELECT generation FROM candidates
			UNION ALL SELECT installation_epoch FROM stores
		)
	`).Scan(&maxGen); err != nil {
		return err
	}
	initial := int64(0)
	if maxGen.Valid {
		initial = maxGen.Int64 + 1
	}
	if _, err := tx.Exec(`
		CREATE TABLE generation_counter (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			value INTEGER NOT NULL
		)
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO generation_counter (id, value) VALUES (1, ?)`, initial); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		CREATE TABLE stores_v3 (
			id INTEGER PRIMARY KEY,
			shop TEXT NOT NULL,
			password TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);
		INSERT INTO stores_v3 (id, shop, password, title, generation, created_at)
		SELECT id, shop, password, title, generation, created_at FROM stores;

		CREATE TABLE candidates_v3 (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL,
			shop TEXT NOT NULL,
			password TEXT NOT NULL,
			generation INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);
	`); err != nil {
		return err
	}

	rows, err := tx.Query(`
		SELECT id, store_id, shop, password, generation, created_at
		FROM candidates
		WHERE datetime(created_at) >= datetime('now', '-7 days')
		ORDER BY shop, datetime(created_at) DESC, generation DESC, id DESC
	`)
	if err != nil {
		return err
	}
	kept := map[string]int{}
	type candRow struct {
		id, shop, password, created string
		storeID                     int
		generation                  int64
	}
	var keep []candRow
	for rows.Next() {
		var c candRow
		if err := rows.Scan(&c.id, &c.storeID, &c.shop, &c.password, &c.generation, &c.created); err != nil {
			return closeRows(rows, err)
		}
		if kept[c.shop] >= MaxCandidatesPerShop {
			continue
		}
		kept[c.shop]++
		keep = append(keep, c)
	}
	if err := rows.Err(); err != nil {
		return closeRows(rows, err)
	}
	if err := closeRows(rows, nil); err != nil {
		return err
	}
	for _, c := range keep {
		if _, err := tx.Exec(`
			INSERT INTO candidates_v3 (id, store_id, shop, password, generation, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, c.id, c.storeID, c.shop, c.password, c.generation, c.created); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`
		DROP TABLE sessions;
		DROP TABLE challenges;
		DROP TABLE candidates;
		DROP TABLE stores;
		ALTER TABLE stores_v3 RENAME TO stores;
		ALTER TABLE candidates_v3 RENAME TO candidates;
		CREATE TABLE challenges (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
			nonce TEXT NOT NULL,
			browser_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			store_id INTEGER NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX idx_candidates_shop ON candidates(shop, generation);
		CREATE INDEX idx_candidates_store ON candidates(store_id);
		CREATE INDEX idx_sessions_store ON sessions(store_id);
		CREATE INDEX idx_challenges_store ON challenges(store_id);
	`); err != nil {
		return err
	}
	if err := s.recordVersion(tx, 3); err != nil {
		return err
	}
	return tx.Commit()
}

// AppliedVersions returns schema versions recorded in the database.
// A missing schema_version table yields an empty slice.
func (s *DB) AppliedVersions() ([]int, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *DB) encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, s.aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := s.aesGCM.Seal(nonce, nonce, plaintext, nil)
	return hex.EncodeToString(ciphertext), nil
}

func (s *DB) decrypt(cipherHex string) ([]byte, error) {
	ciphertext, err := hex.DecodeString(cipherHex)
	if err != nil {
		return nil, fmt.Errorf("decode hex: %w", err)
	}
	nonceSize := s.aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := s.aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

func passwordsEqual(plain string, decrypted []byte) bool {
	return subtle.ConstantTimeCompare([]byte(plain), decrypted) == 1
}

func parseDBTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", value, time.UTC)
}

func (s *DB) reserveGeneration(tx *sql.Tx) (int64, error) {
	var gen int64
	err := tx.QueryRow(`
		UPDATE generation_counter
		SET value = value + 1
		WHERE id = 1
		RETURNING value
	`).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("reserve generation: %w", err)
	}
	return gen, nil
}

// SaveInstall stores an install candidate or keeps the existing matching credential.
func (s *DB) SaveInstall(candidateID string, storeID int, shop, password string) (InstallSave, error) {
	passEnc, err := s.encrypt([]byte(password))
	if err != nil {
		return InstallSave{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return InstallSave{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.deleteExpiredCandidates(tx, shop); err != nil {
		return InstallSave{}, err
	}

	active, err := s.readStoreTx(tx, storeID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return InstallSave{}, err
	}
	if active != nil && active.Shop == shop && passwordsEqual(password, []byte(active.Password)) {
		if err := tx.Commit(); err != nil {
			return InstallSave{}, err
		}
		return InstallSave{Generation: active.Generation, Created: false}, nil
	}

	rows, err := s.candidatesForShop(tx, shop)
	if err != nil {
		return InstallSave{}, err
	}
	var keeper *Candidate
	for i := range rows {
		if passwordsEqual(password, []byte(rows[i].Password)) {
			if keeper == nil || rows[i].Generation > keeper.Generation {
				copyRow := rows[i]
				keeper = &copyRow
			}
		}
	}
	if keeper != nil {
		for _, row := range rows {
			if row.ID != keeper.ID && passwordsEqual(password, []byte(row.Password)) {
				if _, err := tx.Exec(`DELETE FROM candidates WHERE id = ?`, row.ID); err != nil {
					return InstallSave{}, err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return InstallSave{}, err
		}
		return InstallSave{CandidateID: keeper.ID, Generation: keeper.Generation, Created: false}, nil
	}

	if len(rows) >= MaxCandidatesPerShop {
		return InstallSave{}, ErrCapacity
	}
	gen, err := s.reserveGeneration(tx)
	if err != nil {
		return InstallSave{}, err
	}
	createdAt := s.now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`
		INSERT INTO candidates (id, store_id, shop, password, generation, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, candidateID, storeID, shop, passEnc, gen, createdAt); err != nil {
		return InstallSave{}, fmt.Errorf("insert candidate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return InstallSave{}, err
	}
	return InstallSave{CandidateID: candidateID, Generation: gen, Created: true}, nil
}

func (s *DB) deleteExpiredCandidates(tx *sql.Tx, shop string) error {
	rows, err := tx.Query(`SELECT id, created_at FROM candidates WHERE shop = ?`, shop)
	if err != nil {
		return err
	}
	defer rows.Close()
	cutoff := s.now().Add(-CandidateTTL)
	var drop []string
	for rows.Next() {
		var id, createdRaw string
		if err := rows.Scan(&id, &createdRaw); err != nil {
			return err
		}
		created, err := parseDBTime(createdRaw)
		if err != nil {
			return err
		}
		if created.Before(cutoff) {
			drop = append(drop, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range drop {
		if _, err := tx.Exec(`DELETE FROM candidates WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *DB) candidatesForShop(tx *sql.Tx, shop string) ([]Candidate, error) {
	rows, err := tx.Query(`
		SELECT id, store_id, shop, password, generation, created_at
		FROM candidates WHERE shop = ?
	`, shop)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows, s)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCandidate(rows rowScanner, s *DB) (Candidate, error) {
	var c Candidate
	var passEnc, createdRaw string
	if err := rows.Scan(&c.ID, &c.StoreID, &c.Shop, &passEnc, &c.Generation, &createdRaw); err != nil {
		return Candidate{}, err
	}
	plain, err := s.decrypt(passEnc)
	if err != nil {
		return Candidate{}, err
	}
	c.Password = string(plain)
	c.CreatedAt, err = parseDBTime(createdRaw)
	if err != nil {
		return Candidate{}, err
	}
	return c, nil
}

// NewestPending returns the newest unexpired candidate for the store and shop.
func (s *DB) NewestPending(storeID int, shop string) (*Candidate, error) {
	rows, err := s.db.Query(`
		SELECT id, store_id, shop, password, generation, created_at
		FROM candidates
		WHERE store_id = ? AND shop = ?
	`, storeID, shop)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cutoff := s.now().Add(-CandidateTTL)
	var best *Candidate
	for rows.Next() {
		c, err := scanCandidate(rows, s)
		if err != nil {
			return nil, err
		}
		if c.CreatedAt.Before(cutoff) {
			continue
		}
		if best == nil || c.Generation > best.Generation {
			copyC := c
			best = &copyC
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return best, nil
}

// HasOtherShop reports whether this store id is already bound to a different domain.
func (s *DB) HasOtherShop(storeID int, shop string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM stores WHERE id = ? AND shop != ?`, storeID, shop).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM candidates WHERE store_id = ? AND shop != ?`, storeID, shop).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// CountCandidates returns how many candidates exist for a shop.
func (s *DB) CountCandidates(shop string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM candidates WHERE shop = ?`, shop).Scan(&n)
	return n, err
}

// ActivateCandidate promotes a candidate when it is still the newest installation.
// activated is false when the row was removed or a newer connection is already active.
func (s *DB) ActivateCandidate(candidateID, title string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var c Candidate
	var passEnc, createdRaw string
	err = tx.QueryRow(`
		SELECT id, store_id, shop, password, generation, created_at
		FROM candidates WHERE id = ?
	`, candidateID).Scan(&c.ID, &c.StoreID, &c.Shop, &passEnc, &c.Generation, &createdRaw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	created, err := parseDBTime(createdRaw)
	if err != nil {
		return false, err
	}
	if created.Before(s.now().Add(-CandidateTTL)) {
		if _, err := tx.Exec(`DELETE FROM candidates WHERE id = ?`, candidateID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}

	var activeGen sql.NullInt64
	if err := tx.QueryRow(`SELECT generation FROM stores WHERE id = ?`, c.StoreID).Scan(&activeGen); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if activeGen.Valid && c.Generation <= activeGen.Int64 {
		return false, nil
	}

	if _, err := tx.Exec(`DELETE FROM sessions WHERE store_id = ?`, c.StoreID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM challenges WHERE store_id = ?`, c.StoreID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`
		INSERT INTO stores (id, shop, password, title, generation, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			shop = excluded.shop,
			password = excluded.password,
			title = excluded.title,
			generation = excluded.generation,
			created_at = excluded.created_at
	`, c.StoreID, c.Shop, passEnc, title, c.Generation, s.now().UTC().Format(time.RFC3339)); err != nil {
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM candidates WHERE id = ? AND generation = ?`, candidateID, c.Generation)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	return true, tx.Commit()
}

// DeleteCandidate removes one pending install.
func (s *DB) DeleteCandidate(candidateID string) error {
	_, err := s.db.Exec(`DELETE FROM candidates WHERE id = ?`, candidateID)
	return err
}

// GetActiveStore returns the verified connection for an insales id.
func (s *DB) GetActiveStore(storeID int) (*Store, error) {
	return s.readStore(storeID)
}

func (s *DB) readStore(storeID int) (*Store, error) {
	var store Store
	var passEnc, createdRaw string
	err := s.db.QueryRow(`
		SELECT id, shop, password, title, generation, created_at
		FROM stores WHERE id = ?
	`, storeID).Scan(&store.ID, &store.Shop, &passEnc, &store.Title, &store.Generation, &createdRaw)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(passEnc)
	if err != nil {
		return nil, err
	}
	store.Password = string(plain)
	store.CreatedAt, err = parseDBTime(createdRaw)
	if err != nil {
		return nil, err
	}
	return &store, nil
}

func (s *DB) readStoreTx(tx *sql.Tx, storeID int) (*Store, error) {
	var store Store
	var passEnc, createdRaw string
	err := tx.QueryRow(`
		SELECT id, shop, password, title, generation, created_at
		FROM stores WHERE id = ?
	`, storeID).Scan(&store.ID, &store.Shop, &passEnc, &store.Title, &store.Generation, &createdRaw)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(passEnc)
	if err != nil {
		return nil, err
	}
	store.Password = string(plain)
	store.CreatedAt, err = parseDBTime(createdRaw)
	if err != nil {
		return nil, err
	}
	return &store, nil
}

// GetStoreByShop returns a verified connection by canonical shop domain.
func (s *DB) GetStoreByShop(shop string) (*Store, error) {
	var id int
	err := s.db.QueryRow(`SELECT id FROM stores WHERE shop = ?`, shop).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.readStore(id)
}

// ListStores returns verified connections.
func (s *DB) ListStores() ([]Store, error) {
	rows, err := s.db.Query(`SELECT id FROM stores ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Store
	for _, id := range ids {
		store, err := s.readStore(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *store)
	}
	return out, nil
}

// Uninstall removes the connection that matches insales id, shop and credential.
// Pending rows are removed only when the password matches, so a later install
// with another password survives a delayed uninstall of the previous one.
// removed is false when nothing matched. ErrNotAuthorized means the pair exists
// but the password does not.
func (s *DB) Uninstall(storeID int, shop, password string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	active, err := s.readStoreTx(tx, storeID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	activeHere := active != nil && active.Shop == shop
	activeMatch := activeHere && passwordsEqual(password, []byte(active.Password))

	rows, err := tx.Query(`SELECT id, password FROM candidates WHERE store_id = ? AND shop = ?`, storeID, shop)
	if err != nil {
		return false, err
	}
	var matched []string
	hadPending := false
	for rows.Next() {
		hadPending = true
		var id, passEnc string
		if err := rows.Scan(&id, &passEnc); err != nil {
			return false, closeRows(rows, err)
		}
		plain, err := s.decrypt(passEnc)
		if err != nil {
			return false, closeRows(rows, err)
		}
		if passwordsEqual(password, plain) {
			matched = append(matched, id)
		}
	}
	if err := rows.Err(); err != nil {
		return false, closeRows(rows, err)
	}
	if err := closeRows(rows, nil); err != nil {
		return false, err
	}

	if !activeMatch && len(matched) == 0 {
		if activeHere || hadPending {
			return false, ErrNotAuthorized
		}
		return false, nil
	}
	for _, id := range matched {
		if _, err := tx.Exec(`DELETE FROM candidates WHERE id = ?`, id); err != nil {
			return false, err
		}
	}
	if activeMatch {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE store_id = ?`, storeID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM challenges WHERE store_id = ?`, storeID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM stores WHERE id = ?`, storeID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *DB) cleanupExpired(tx *sql.Tx) error {
	now := s.now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`DELETE FROM challenges WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	return nil
}

// CreateChallenge stores a nonce bound to the store's current generation.
func (s *DB) CreateChallenge(id, nonce, browserID string, storeID int, ttl time.Duration) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := s.cleanupExpired(tx); err != nil {
		return 0, err
	}
	expires := s.now().Add(ttl).UTC().Format(time.RFC3339)
	created := s.now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(`
		INSERT INTO challenges (id, store_id, nonce, browser_id, generation, expires_at, created_at)
		SELECT ?, id, ?, ?, generation, ?, ?
		FROM stores
		WHERE id = ?
	`, id, nonce, browserID, expires, created, storeID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, ErrNotFound
	}
	var gen int64
	if err := tx.QueryRow(`SELECT generation FROM challenges WHERE id = ?`, id).Scan(&gen); err != nil {
		return 0, err
	}
	return gen, tx.Commit()
}

// ConsumeChallenge reads and deletes a challenge bound to the browser and current generation.
func (s *DB) ConsumeChallenge(id, browserID string) (*Challenge, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var ch Challenge
	var expiresRaw string
	err = tx.QueryRow(`
		SELECT id, store_id, nonce, browser_id, generation, expires_at
		FROM challenges WHERE id = ?
	`, id).Scan(&ch.ID, &ch.StoreID, &ch.Nonce, &ch.BrowserID, &ch.Generation, &expiresRaw)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ch.ExpiresAt, err = parseDBTime(expiresRaw)
	if err != nil {
		return nil, err
	}
	if !s.now().Before(ch.ExpiresAt) {
		if _, err := tx.Exec(`DELETE FROM challenges WHERE id = ?`, id); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if ch.BrowserID != browserID {
		return nil, ErrNotAuthorized
	}
	var storeGen int64
	err = tx.QueryRow(`SELECT generation FROM stores WHERE id = ?`, ch.StoreID).Scan(&storeGen)
	if err == sql.ErrNoRows || (err == nil && storeGen != ch.Generation) {
		if _, delErr := tx.Exec(`DELETE FROM challenges WHERE id = ?`, id); delErr != nil {
			return nil, delErr
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrStale
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM challenges WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &ch, tx.Commit()
}

// CreateSession stores a session for the generation that is still current.
func (s *DB) CreateSession(id string, storeID int, userID string, generation int64, ttl time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.cleanupExpired(tx); err != nil {
		return err
	}
	expires := s.now().Add(ttl).UTC().Format(time.RFC3339)
	created := s.now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(`
		INSERT INTO sessions (id, store_id, user_id, generation, expires_at, created_at)
		SELECT ?, id, ?, generation, ?, ?
		FROM stores
		WHERE id = ? AND generation = ?
	`, id, userID, expires, created, storeID, generation)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrStale
	}
	return tx.Commit()
}

// VerifySession loads a session and its store when the generation still matches.
func (s *DB) VerifySession(sessionID string) (*Session, *Store, error) {
	var sess Session
	var store Store
	var passEnc, expiresRaw, createdRaw string
	err := s.db.QueryRow(`
		SELECT s.id, s.store_id, s.user_id, s.generation, s.expires_at,
		       st.shop, st.password, st.title, st.generation, st.created_at
		FROM sessions s
		JOIN stores st ON st.id = s.store_id AND st.generation = s.generation
		WHERE s.id = ?
	`, sessionID).Scan(
		&sess.ID, &sess.StoreID, &sess.UserID, &sess.Generation, &expiresRaw,
		&store.Shop, &passEnc, &store.Title, &store.Generation, &createdRaw,
	)
	if err == sql.ErrNoRows {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	sess.ExpiresAt, err = parseDBTime(expiresRaw)
	if err != nil {
		return nil, nil, err
	}
	if !s.now().Before(sess.ExpiresAt) {
		return nil, nil, ErrNotFound
	}
	plain, err := s.decrypt(passEnc)
	if err != nil {
		return nil, nil, err
	}
	store.ID = sess.StoreID
	store.Password = string(plain)
	store.CreatedAt, err = parseDBTime(createdRaw)
	if err != nil {
		return nil, nil, err
	}
	return &sess, &store, nil
}

// DeleteSession removes one session.
func (s *DB) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// GenerationValue returns the last reserved generation, or 0 when the counter is absent.
func (s *DB) GenerationValue() (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT value FROM generation_counter WHERE id = 1`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	return v, err
}
