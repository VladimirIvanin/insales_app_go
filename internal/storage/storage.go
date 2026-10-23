// Package storage provides SQLite-backed persistence for InSales app state.
//
// This implementation uses a single SQLite database for all state:
//   - Stores (shop installations with credentials)
//   - Sessions (authenticated browser sessions)
//   - Challenges (one-time login nonces)
//   - Candidates (pending installation verifications)
//
// First version: single process, single file. Not designed for horizontal scaling.
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

	"log/slog"

	_ "modernc.org/sqlite"
)

// Common errors.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrNotAuthorized = errors.New("not authorized")
)

// Store represents an installed shop.
type Store struct {
	ID                int    // insales_id
	Shop              string // canonical shop domain
	Password          string // encrypted install password
	Title             string // shop title from API
	Generation        int64  // monotonic generation counter for credential rotation
	InstallationEpoch int64  // monotonic epoch, never reset — prevents cross-installation session reuse
	CreatedAt         time.Time
}

// Session represents an authenticated browser session.
type Session struct {
	ID                string
	StoreID           int
	UserID            string
	Generation        int64
	InstallationEpoch int64
	ExpiresAt         time.Time
}

// Challenge represents a one-time login nonce.
type Challenge struct {
	ID        string
	StoreID   int
	Nonce     string
	BrowserID string
	ExpiresAt time.Time
}

// Candidate represents a pending installation being verified.
type Candidate struct {
	ID          string
	StoreID     int // insales_id from install callback
	Shop        string
	Password    string // encrypted password
	Generation  int64
	Attempts    int
	MaxAttempts int
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DB wraps the SQLite database connection and provides storage operations.
type DB struct {
	db     *sql.DB
	aesGCM cipher.AEAD
	now    func() time.Time
}

// New opens or creates a SQLite database, runs migrations, and returns a DB handle.
// PRAGMA settings (WAL, busy_timeout, foreign_keys) are applied via Exec after open,
// because modernc.org/sqlite does not honour DSN query parameters.
func New(dbPath string, encryptionKey []byte) (*DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Apply PRAGMA settings at runtime — DSN parameters are not supported by modernc.org/sqlite.
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	// Create AES-GCM cipher for password encryption
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}

	db.SetMaxOpenConns(1)

	s := &DB{db: db, aesGCM: aesGCM, now: time.Now}

	if err := s.runMigrations(); err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}

	return s, nil
}

// SetClock replaces the time source (used in tests).
func (s *DB) SetClock(now func() time.Time) { s.now = now }

// DB returns the underlying *sql.DB for advanced operations.
func (s *DB) DB() *sql.DB { return s.db }

// Close closes the database connection.
func (s *DB) Close() error { return s.db.Close() }

// runMigrations executes versioned migrations atomically.
func (s *DB) runMigrations() error {
	// Create migration tracking table
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	migrations := []struct {
		version int
		sql     string
	}{
		{
			version: 1,
			sql: `
				PRAGMA foreign_keys = ON;

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
			`,
		},
		{
			version: 2,
			sql: `
				ALTER TABLE stores ADD COLUMN installation_epoch INTEGER NOT NULL DEFAULT 1;
				ALTER TABLE sessions ADD COLUMN installation_epoch INTEGER NOT NULL DEFAULT 1;
				CREATE INDEX IF NOT EXISTS idx_stores_epoch ON stores(installation_epoch);
			`,
		},
	}

	for _, m := range migrations {
		var exists int
		err := s.db.QueryRow("SELECT COUNT(*) FROM schema_version WHERE version = ?", m.version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if exists > 0 {
			continue
		}

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", m.version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}

	return nil
}

// encrypt encrypts plaintext using AES-GCM and returns hex-encoded ciphertext.
func (s *DB) encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, s.aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := s.aesGCM.Seal(nonce, nonce, plaintext, nil)
	return hex.EncodeToString(ciphertext), nil
}

// decrypt decrypts hex-encoded AES-GCM ciphertext.
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

// --- Store operations ---

// CreateCandidate saves a pending installation candidate.
// Returns ErrAlreadyExists if a candidate with the same ID already exists.
func (s *DB) CreateCandidate(c *Candidate) error {
	passEnc, err := s.encrypt([]byte(c.Password))
	if err != nil {
		return err
	}

	// Enforce per-store candidate limit
	var storeCount int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM candidates WHERE store_id = ?", c.StoreID).Scan(&storeCount); err != nil {
		return fmt.Errorf("count candidates: %w", err)
	}
	if storeCount >= 5 {
		return fmt.Errorf("too many pending candidates for store %d (max 5)", c.StoreID)
	}

	_, err = s.db.Exec(`
		INSERT INTO candidates (id, store_id, shop, password, generation, max_attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, c.ID, c.StoreID, c.Shop, passEnc, c.Generation, c.MaxAttempts)
	if err != nil {
		return fmt.Errorf("insert candidate: %w", err)
	}
	return nil
}

// CreateCandidateAtomic atomically reserves the next generation and inserts a candidate.
// Uses a transaction to prevent race conditions on generation assignment.
// Enforces per-store and global candidate limits.
func (s *DB) CreateCandidateAtomic(c *Candidate) error {
	passEnc, err := s.encrypt([]byte(c.Password))
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Enforce per-store candidate limit
	var storeCount int
	if err := tx.QueryRow("SELECT COUNT(*) FROM candidates WHERE store_id = ?", c.StoreID).Scan(&storeCount); err != nil {
		return fmt.Errorf("count candidates: %w", err)
	}
	if storeCount >= 5 {
		return fmt.Errorf("too many pending candidates for store %d (max 5)", c.StoreID)
	}

	// Enforce global candidate limit
	var globalCount int
	if err := tx.QueryRow("SELECT COUNT(*) FROM candidates").Scan(&globalCount); err != nil {
		return fmt.Errorf("count global candidates: %w", err)
	}
	if globalCount >= 100 {
		return fmt.Errorf("too many pending candidates globally (max 100)")
	}

	// Atomically determine next generation under lock
	var gen int64
	err = tx.QueryRow(`
		SELECT COALESCE(MAX(generation), 0) FROM (
			SELECT generation FROM stores WHERE id = ?
			UNION ALL
			SELECT generation FROM candidates WHERE store_id = ?
		)
	`, c.StoreID, c.StoreID).Scan(&gen)
	if err != nil {
		return fmt.Errorf("get max generation: %w", err)
	}
	newGen := gen + 1

	_, err = tx.Exec(`
		INSERT INTO candidates (id, store_id, shop, password, generation, max_attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, c.ID, c.StoreID, c.Shop, passEnc, newGen, c.MaxAttempts)
	if err != nil {
		return fmt.Errorf("insert candidate: %w", err)
	}

	c.Generation = newGen

	return tx.Commit()
}

// ClaimCandidate atomically claims a candidate for verification.
// Uses a transaction to prevent concurrent claims.
// Sets a lease token and next_attempt_at for retry/backoff.
// If no candidate is available, returns nil, nil.
func (s *DB) ClaimCandidate() (*Candidate, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Pick the next candidate with attempts remaining and lease expired
	row := tx.QueryRow(`
		SELECT id, store_id, shop, password, generation, attempts, max_attempts, last_error, created_at, updated_at
		FROM candidates
		WHERE attempts < max_attempts
		  AND (next_attempt_at IS NULL OR next_attempt_at <= datetime('now'))
		ORDER BY created_at ASC
		LIMIT 1
	`)

	var c Candidate
	var passEnc string
	var createdAtStr, updatedAtStr string
	var lastError sql.NullString
	err = row.Scan(&c.ID, &c.StoreID, &c.Shop, &passEnc, &c.Generation, &c.Attempts, &c.MaxAttempts, &lastError, &createdAtStr, &updatedAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan candidate: %w", err)
	}

	if lastError.Valid {
		c.LastError = lastError.String
	}

	plainPass, err := s.decrypt(passEnc)
	if err != nil {
		return nil, err
	}
	c.Password = string(plainPass)
	c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
	c.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAtStr)

	// Generate unique lease token
	leaseToken := randomHex(16)

	// Set next_attempt_at with exponential backoff (10s, 30s, 90s)
	var backoffSeconds int
	switch c.Attempts {
	case 0:
		backoffSeconds = 10
	case 1:
		backoffSeconds = 30
	default:
		backoffSeconds = 90
	}

	_, err = tx.Exec(`
		UPDATE candidates
		SET attempts = attempts + 1,
		    lease_token = ?,
		    next_attempt_at = datetime('now', '+' || ? || ' seconds'),
		    updated_at = datetime('now')
		WHERE id = ?
	`, leaseToken, backoffSeconds, c.ID)
	if err != nil {
		return nil, fmt.Errorf("update lease: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}

	return &c, nil
}

// randomHex returns cryptographically random hex string of n bytes.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate random: %v", err))
	}
	return hex.EncodeToString(b)
}

// ReleaseLease releases the lease on a candidate, resetting attempt count
// for retry on transient errors.
func (s *DB) ReleaseLease(candidateID string) error {
	_, err := s.db.Exec(`
		UPDATE candidates
		SET lease_token = '',
		    attempts = GREATEST(attempts - 1, 0),
		    updated_at = datetime('now')
		WHERE id = ?
	`, candidateID)
	return err
}

// FailCandidate records a transient failure for a candidate.
// Sets last_error and schedules retry via next_attempt_at.
func (s *DB) FailCandidate(candidateID, lastError string, retryAfter int) error {
	_, err := s.db.Exec(`
		UPDATE candidates
		SET last_error = ?,
		    next_attempt_at = datetime('now', '+' || ? || ' seconds'),
		    updated_at = datetime('now')
		WHERE id = ?
	`, lastError, retryAfter, candidateID)
	return err
}

// CompleteCandidate removes a candidate after successful promotion.
func (s *DB) CompleteCandidate(candidateID string) error {
	_, err := s.db.Exec("DELETE FROM candidates WHERE id = ?", candidateID)
	return err
}

// PromoteCandidate moves a verified candidate to the stores table.
// It atomically checks that the candidate is still valid (not cancelled),
// and that the generation matches or is newer than the current store.
func (s *DB) PromoteCandidate(candidateID string, title string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Re-read candidate within transaction
	var c struct {
		id         string
		storeID    int
		shop       string
		passEnc    string
		generation int64
	}
	err = tx.QueryRow(`
		SELECT id, store_id, shop, password, generation FROM candidates WHERE id = ?
	`, candidateID).Scan(&c.id, &c.storeID, &c.shop, &c.passEnc, &c.generation)
	if err == sql.ErrNoRows {
		return false, nil // cancelled or already processed
	}
	if err != nil {
		return false, fmt.Errorf("re-read candidate: %w", err)
	}

	// Check current store generation (if exists)
	var currentGen int64
	err = tx.QueryRow("SELECT COALESCE(MAX(generation), 0) FROM stores WHERE id = ?", c.storeID).Scan(&currentGen)
	if err != nil {
		return false, fmt.Errorf("check current gen: %w", err)
	}

	// Only promote if our generation >= current (candidate is newer or equal)
	if c.generation < currentGen {
		slog.Debug("candidate generation older than current, skipping", "store", c.storeID, "candidateGen", c.generation, "currentGen", currentGen)
		return false, nil
	}

	// Upsert store — increment installation_epoch on reinstall to prevent
	// cross-installation session reuse.
	var currentEpoch int64
	err = tx.QueryRow("SELECT COALESCE(MAX(installation_epoch), 0) FROM stores WHERE id = ?", c.storeID).Scan(&currentEpoch)
	if err != nil {
		return false, fmt.Errorf("check current epoch: %w", err)
	}
	newEpoch := currentEpoch + 1

	_, err = tx.Exec(`
		INSERT INTO stores (id, shop, password, title, generation, installation_epoch, created_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			shop = excluded.shop,
			password = excluded.password,
			title = excluded.title,
			generation = excluded.generation,
			installation_epoch = excluded.installation_epoch,
			created_at = datetime('now')
	`, c.storeID, c.shop, c.passEnc, title, c.generation, newEpoch)
	if err != nil {
		return false, fmt.Errorf("upsert store: %w", err)
	}

	// Delete candidate
	_, err = tx.Exec("DELETE FROM candidates WHERE id = ?", candidateID)
	if err != nil {
		return false, fmt.Errorf("delete candidate: %w", err)
	}

	return true, tx.Commit()
}

// CancelCandidate removes candidates for a store that have a lower generation,
// or all candidates matching storeID+generation.
func (s *DB) CancelCandidate(storeID int, generation int64) error {
	_, err := s.db.Exec("DELETE FROM candidates WHERE store_id = ? AND generation <= ?", storeID, generation)
	return err
}

// GetActiveStore returns the active store for the given insales_id.
func (s *DB) GetActiveStore(storeID int) (*Store, error) {
	var store Store
	var passEnc string
	var createdAtStr string
	err := s.db.QueryRow(`
		SELECT id, shop, password, title, generation, installation_epoch, created_at
		FROM stores WHERE id = ?
	`, storeID).Scan(&store.ID, &store.Shop, &passEnc, &store.Title, &store.Generation, &store.InstallationEpoch, &createdAtStr)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get store: %w", err)
	}

	plainPass, err := s.decrypt(passEnc)
	if err != nil {
		return nil, err
	}
	store.Password = string(plainPass)
	store.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)

	return &store, nil
}

// GetStoreByShop returns a store by its shop domain.
func (s *DB) GetStoreByShop(shop string) (*Store, error) {
	var store Store
	var passEnc string
	var createdAtStr string
	err := s.db.QueryRow(`
		SELECT id, shop, password, title, generation, installation_epoch, created_at
		FROM stores WHERE shop = ?
	`, shop).Scan(&store.ID, &store.Shop, &passEnc, &store.Title, &store.Generation, &store.InstallationEpoch, &createdAtStr)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get store by shop: %w", err)
	}

	plainPass, err := s.decrypt(passEnc)
	if err != nil {
		return nil, err
	}
	store.Password = string(plainPass)
	store.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)

	return &store, nil
}

// UninstallStore removes a store and all its related data in a single write-transaction.
//
// It verifies the password against the ACTIVE store first (full removal of store +
// sessions + challenges + candidates). If the password matches a CANDIDATE instead
// (pending verification), only that candidate and its matching store data are removed.
//
// The entire check and deletion happens atomically inside one write-transaction,
// preventing TOCTOU races (finding #4). Blanket delete by store_id is only performed
// when the active credential matches (fixing #5). All candidates are scanned for a
// password match, not just the latest (fixing #6).
//
// Returns (removed, error). removed=false with nil error means nothing found (idempotent).
func (s *DB) UninstallStore(storeID int, password string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Check active store password inside transaction
	var activePassEnc sql.NullString
	err = tx.QueryRow("SELECT password FROM stores WHERE id = ?", storeID).Scan(&activePassEnc)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("read active store password: %w", err)
	}

	activeMatch := false
	if activePassEnc.Valid {
		plainPass, decErr := s.decrypt(activePassEnc.String)
		if decErr != nil {
			return false, fmt.Errorf("decrypt active password: %w", decErr)
		}
		if subtle.ConstantTimeCompare(plainPass, []byte(password)) == 1 {
			activeMatch = true
		}
	}

	if activeMatch {
		// Full uninstall — delete everything for this store
		if _, err := tx.Exec("DELETE FROM candidates WHERE store_id = ?", storeID); err != nil {
			return false, fmt.Errorf("delete candidates: %w", err)
		}
		if _, err := tx.Exec("DELETE FROM challenges WHERE store_id = ?", storeID); err != nil {
			return false, fmt.Errorf("delete challenges: %w", err)
		}
		if _, err := tx.Exec("DELETE FROM sessions WHERE store_id = ?", storeID); err != nil {
			return false, fmt.Errorf("delete sessions: %w", err)
		}
		if _, err := tx.Exec("DELETE FROM stores WHERE id = ?", storeID); err != nil {
			return false, fmt.Errorf("delete store: %w", err)
		}
		return true, tx.Commit()
	}

	// 2. Check all candidates for a password match (not just the latest generation)
	var candidateIDs []string
	var hadCandidates bool
	rows, err := tx.Query("SELECT id, password FROM candidates WHERE store_id = ?", storeID)
	if err != nil {
		return false, fmt.Errorf("read candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		hadCandidates = true
		var cID, cPassEnc string
		if err := rows.Scan(&cID, &cPassEnc); err != nil {
			return false, fmt.Errorf("scan candidate: %w", err)
		}
		plainPass, decErr := s.decrypt(cPassEnc)
		if decErr != nil {
			return false, fmt.Errorf("decrypt candidate password: %w", decErr)
		}
		if subtle.ConstantTimeCompare(plainPass, []byte(password)) == 1 {
			candidateIDs = append(candidateIDs, cID)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("rows iteration: %w", err)
	}

	if len(candidateIDs) > 0 {
		// Cancel matching candidates only — do NOT touch active store or other credentials
		for _, cid := range candidateIDs {
			if _, err := tx.Exec("DELETE FROM candidates WHERE id = ?", cid); err != nil {
				return false, fmt.Errorf("delete candidate %s: %w", cid, err)
			}
		}
		return true, tx.Commit()
	}

	// 3. Nothing matched
	if activePassEnc.Valid {
		// Active store exists but wrong password, no matching candidates
		return false, ErrNotAuthorized
	}

	if hadCandidates {
		// Candidates exist but none matched the password
		return false, ErrNotAuthorized
	}

	// No store, no candidates — idempotent
	return false, nil
}

// --- Session operations ---

// CreateSession creates a new browser session.
func (s *DB) CreateSession(id string, storeID int, userID string, generation int64, installationEpoch int64, ttl time.Duration) error {
	expires := s.now().Add(ttl)
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, store_id, user_id, generation, installation_epoch, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
	`, id, storeID, userID, generation, installationEpoch, expires.Format(time.RFC3339))
	return err
}

// GetSession retrieves a session and checks it hasn't expired.
func (s *DB) GetSession(id string) (*Session, error) {
	var sess Session
	var expiresStr string
	err := s.db.QueryRow(`
		SELECT s.id, s.store_id, s.user_id, s.generation, s.installation_epoch, s.expires_at
		FROM sessions s
		WHERE s.id = ?
	`, id).Scan(&sess.ID, &sess.StoreID, &sess.UserID, &sess.Generation, &sess.InstallationEpoch, &expiresStr)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	sess.ExpiresAt, err = time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return nil, fmt.Errorf("parse expires: %w", err)
	}

	if s.now().After(sess.ExpiresAt) {
		// Clean up expired session
		s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
		return nil, ErrNotFound
	}

	return &sess, nil
}

// DeleteSession removes a single session.
func (s *DB) DeleteSession(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

// DeleteStoreSessions removes all sessions for a store.
func (s *DB) DeleteStoreSessions(storeID int) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE store_id = ?", storeID)
	return err
}

// --- Challenge operations ---

// CreateChallenge creates a one-time login challenge.
func (s *DB) CreateChallenge(id string, storeID int, nonce string, browserID string, ttl time.Duration) error {
	expires := s.now().Add(ttl)
	_, err := s.db.Exec(`
		INSERT INTO challenges (id, store_id, nonce, browser_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
	`, id, storeID, nonce, browserID, expires.Format(time.RFC3339))
	return err
}

// ConsumeChallenge atomically reads and deletes a challenge.
// Returns the challenge if it exists, hasn't expired, and is bound to the given browser.
func (s *DB) ConsumeChallenge(id string, browserID string) (*Challenge, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var ch Challenge
	var expiresStr string
	err = tx.QueryRow(`
		SELECT id, store_id, nonce, browser_id, expires_at
		FROM challenges WHERE id = ?
	`, id).Scan(&ch.ID, &ch.StoreID, &ch.Nonce, &ch.BrowserID, &expiresStr)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan challenge: %w", err)
	}

	ch.ExpiresAt, err = time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return nil, fmt.Errorf("parse expires: %w", err)
	}

	// Check expiry
	if s.now().After(ch.ExpiresAt) {
		if _, err := tx.Exec("DELETE FROM challenges WHERE id = ?", id); err != nil {
			return nil, fmt.Errorf("delete expired challenge: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit delete expired challenge: %w", err)
		}
		return nil, ErrNotFound
	}

	// Check browser binding
	if ch.BrowserID != browserID {
		return nil, ErrNotAuthorized
	}

	// Atomically delete (consume) the challenge
	result, err := tx.Exec("DELETE FROM challenges WHERE id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("delete challenge: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil, ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &ch, nil
}

// DeleteStoreChallenges removes all challenges for a store.
func (s *DB) DeleteStoreChallenges(storeID int) error {
	_, err := s.db.Exec("DELETE FROM challenges WHERE store_id = ?", storeID)
	return err
}

// GetStoreGeneration returns the current generation for a store.
func (s *DB) GetStoreGeneration(storeID int) (int64, error) {
	var gen int64
	err := s.db.QueryRow("SELECT COALESCE(MAX(generation), 0) FROM stores WHERE id = ?", storeID).Scan(&gen)
	return gen, err
}

// GetLastCandidateGeneration returns the latest generation from candidates for a store.
func (s *DB) GetLastCandidateGeneration(storeID int) (int64, error) {
	var gen int64
	err := s.db.QueryRow("SELECT COALESCE(MAX(generation), 0) FROM candidates WHERE store_id = ?", storeID).Scan(&gen)
	return gen, err
}

// CleanupExpired removes expired sessions and challenges.
func (s *DB) CleanupExpired() {
	now := s.now().Format(time.RFC3339)
	s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", now)
	s.db.Exec("DELETE FROM challenges WHERE expires_at < ?", now)
}

// HasStore checks if a store exists by ID.
func (s *DB) HasStore(storeID int) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM stores WHERE id = ?", storeID).Scan(&count)
	return count > 0, err
}
