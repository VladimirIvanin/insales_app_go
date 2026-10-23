// Package lifecycle implements the InSales app installation, login and uninstall flows.
//
// Lifecycle flow:
//  1. Install: receive callback, save candidate, respond 200, verify in background
//  2. Login: generate nonce, redirect to InSales, verify token3 callback
//  3. Uninstall: verify password, remove store and all related data
//
// The lifecycle uses the storage layer for persistence and the InSales client for API calls.
package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

// Session TTL constants.
const (
	ChallengeTTL = 5 * time.Minute
	SessionTTL   = 1 * time.Hour
)

// Errors.
var (
	ErrForbidden         = errors.New("forbidden")
	ErrExpired           = errors.New("expired")
	ErrAlreadyProcessed  = errors.New("already processed")
	ErrStoreNotInstalled = errors.New("store not installed")
	ErrInvalidSignature  = errors.New("invalid signature")
)

// Service handles the application lifecycle.
type Service struct {
	store     *storage.DB
	appID     string
	appSecret string
	newClient func(shop, password string) *insales.Client
	now       func() time.Time
}

// NewService creates a new lifecycle service.
func NewService(store *storage.DB, appID, appSecret string) *Service {
	return &Service{
		store:     store,
		appID:     appID,
		appSecret: appSecret,
		newClient: func(shop, password string) *insales.Client {
			return insales.NewStrictClient(shop, appID, password)
		},
		now: time.Now,
	}
}

// SetClock replaces the time source (used in tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetClientFactory replaces the InSales client factory (used in tests).
func (s *Service) SetClientFactory(f func(shop, password string) *insales.Client) {
	s.newClient = f
}

// NewClient creates a new InSales API client for the given shop and password
// using the configured client factory (which can be replaced in tests).
func (s *Service) NewClient(shop, password string) *insales.Client {
	return s.newClient(shop, password)
}

// InstallParams holds the parameters from the InSales install callback.
type InstallParams struct {
	Shop      string
	Token     string
	InsalesID int
}

// InstallResult describes the result of processing an install callback.
type InstallResult struct {
	Password    string // computed store password
	CandidateID string
}

// HandleInstall processes an install callback from InSales.
// It computes the password, atomically reserves the next generation,
// saves a candidate, and starts background verification.
func (s *Service) HandleInstall(ctx context.Context, params InstallParams) (*InstallResult, error) {
	password := insales.InstallPassword(params.Token, s.appSecret)

	candidateID := randomHex(16)

	candidate := &storage.Candidate{
		ID:          candidateID,
		StoreID:     params.InsalesID,
		Shop:        params.Shop,
		Password:    password,
		MaxAttempts: 3,
	}

	if err := s.store.CreateCandidateAtomic(candidate); err != nil {
		return nil, fmt.Errorf("create candidate: %w", err)
	}

	return &InstallResult{
		Password:    password,
		CandidateID: candidateID,
	}, nil
}

// VerifyInstall verifies a candidate by calling the InSales API and promotes it on success.
func (s *Service) VerifyInstall(ctx context.Context, candidate *storage.Candidate) error {
	slog.Info("verifying install candidate", "store", candidate.StoreID, "shop", candidate.Shop)

	client := s.newClient("https://"+candidate.Shop, candidate.Password)
	account, err := client.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("get account: %w", err)
	}

	if account.ID != candidate.StoreID {
		return fmt.Errorf("account ID mismatch: got %d, want %d", account.ID, candidate.StoreID)
	}

	// Atomically promote candidate to active store
	promoted, err := s.store.PromoteCandidate(candidate.ID, account.Title)
	if err != nil {
		return fmt.Errorf("promote candidate: %w", err)
	}
	if !promoted {
		slog.Info("candidate already cancelled or superseded", "candidate", candidate.ID, "store", candidate.StoreID)
	}

	return nil
}

// UninstallParams holds the parameters from the InSales uninstall callback.
type UninstallParams struct {
	Shop      string
	Token     string // the store password (NOT the install token)
	InsalesID int
}

// HandleUninstall processes an uninstall callback from InSales.
func (s *Service) HandleUninstall(ctx context.Context, params UninstallParams) error {
	// Find the store by shop first to get the store ID
	storeID := params.InsalesID

	if storeID <= 0 {
		store, err := s.store.GetStoreByShop(params.Shop)
		if err != nil {
			return ErrStoreNotInstalled
		}
		storeID = store.ID
	}

	// Verify password and remove in a single transaction
	removed, err := s.store.UninstallStore(storeID, params.Token)
	if err == storage.ErrNotAuthorized {
		return ErrForbidden
	}
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if !removed {
		// Already removed or superseded — idempotent
		slog.Info("uninstall: store already removed or credential mismatch", "store", storeID)
	}

	return nil
}

// LoginParams holds the initial login request parameters from InSales.
type LoginParams struct {
	Shop      string
	InsalesID int
	UserID    string
}

// LoginResult describes the result of initiating a login flow.
type LoginResult struct {
	RedirectURL string // URL to redirect the user's browser to
	ChallengeID string
	Nonce       string // the generated nonce (needed for token3)
}

// HandleLogin initiates a login flow by creating a challenge and returning a redirect URL.
func (s *Service) HandleLogin(params LoginParams, browserID string) (*LoginResult, error) {
	// Verify the store exists
	store, err := s.store.GetActiveStore(params.InsalesID)
	if err != nil {
		if err == storage.ErrNotFound {
			return nil, ErrStoreNotInstalled
		}
		return nil, fmt.Errorf("get store: %w", err)
	}

	if store.Shop != params.Shop {
		return nil, ErrForbidden
	}

	nonce := randomHex(32)
	challengeID := randomHex(16)

	if err := s.store.CreateChallenge(challengeID, params.InsalesID, nonce, browserID, ChallengeTTL); err != nil {
		return nil, fmt.Errorf("create challenge: %w", err)
	}

	return &LoginResult{
		ChallengeID: challengeID,
		Nonce:       nonce,
	}, nil
}

// BuildLoginRedirectURL builds the InSales redirect URL for the login flow.
func BuildLoginRedirectURL(shop, appID, nonce, callbackURL string) string {
	return fmt.Sprintf("https://%s/admin/applications/%s/login?token=%s&login=%s",
		shop, appID, nonce, callbackURL)
}

// AutologinParams holds the parameters from the InSales autologin callback.
type AutologinParams struct {
	ChallengeID    string
	Token          string // token = MD5(nonce + password)
	Token2         string // token2 = MD5(nonce + email + name + id + password)
	Token3         string // token3 = MD5(nonce + email + name + id + email_confirmed + password)
	UserEmail      string
	UserName       string
	UserID         string
	EmailConfirmed string // "true" or "false"
	BrowserID      string
}

// AutologinResult describes the result of processing an autologin callback.
type AutologinResult struct {
	SessionID string
	StoreID   int
	UserID    string
}

// HandleAutologin verifies the token3 from InSales and creates a browser session.
func (s *Service) HandleAutologin(ctx context.Context, params AutologinParams) (*AutologinResult, error) {
	// Consume the challenge atomically
	challenge, err := s.store.ConsumeChallenge(params.ChallengeID, params.BrowserID)
	if err != nil {
		if err == storage.ErrNotFound {
			return nil, ErrExpired
		}
		if err == storage.ErrNotAuthorized {
			return nil, ErrForbidden
		}
		return nil, fmt.Errorf("consume challenge: %w", err)
	}

	// Get the store to retrieve the password
	store, err := s.store.GetActiveStore(challenge.StoreID)
	if err != nil {
		if err == storage.ErrNotFound {
			return nil, ErrStoreNotInstalled
		}
		return nil, fmt.Errorf("get store: %w", err)
	}

	// Verify token3 using constant-time comparison
	expectedToken3 := insales.Token3(
		challenge.Nonce,
		params.UserEmail,
		params.UserName,
		params.UserID,
		params.EmailConfirmed,
		store.Password,
	)

	if subtle.ConstantTimeCompare([]byte(expectedToken3), []byte(params.Token3)) != 1 {
		return nil, ErrInvalidSignature
	}

	// Create session with installation_epoch to bind it to this installation
	sessionID := randomHex(32)
	if err := s.store.CreateSession(sessionID, challenge.StoreID, params.UserID, store.Generation, store.InstallationEpoch, SessionTTL); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	return &AutologinResult{
		SessionID: sessionID,
		StoreID:   challenge.StoreID,
		UserID:    params.UserID,
	}, nil
}

// VerifySession checks that a session is valid and returns the associated store.
// It also verifies that both the store's generation and installation_epoch
// match the session — the epoch check prevents cross-installation session reuse
// after uninstall + reinstall.
func (s *Service) VerifySession(sessionID string) (*storage.Session, *storage.Store, error) {
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("get session: %w", err)
	}

	store, err := s.store.GetActiveStore(sess.StoreID)
	if err != nil {
		return nil, nil, fmt.Errorf("get store: %w", err)
	}

	// Check generation match (store hasn't been reinstalled)
	if sess.Generation != store.Generation {
		return nil, nil, ErrExpired
	}

	// Check installation_epoch match — prevents a session created under a previous
	// installation from being reused after uninstall+reinstall (which resets
	// generation to 1 but increments installation_epoch).
	if sess.InstallationEpoch != store.InstallationEpoch {
		return nil, nil, ErrExpired
	}

	return sess, store, nil
}

// Logout terminates a session.
func (s *Service) Logout(sessionID string) error {
	return s.store.DeleteSession(sessionID)
}

// randomHex returns cryptographically random hex string of n bytes.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate random: %v", err))
	}
	return hex.EncodeToString(b)
}
