package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

const (
	ChallengeTTL  = 5 * time.Minute
	SessionTTL    = 1 * time.Hour
	verifyTimeout = 5 * time.Second
)

var (
	ErrForbidden         = errors.New("forbidden")
	ErrExpired           = errors.New("expired")
	ErrStoreNotInstalled = errors.New("store not installed")
	ErrInvalidSignature  = errors.New("invalid signature")
	ErrTemporary         = errors.New("temporary verification error")
	ErrVerification      = errors.New("verification response invalid")
)

// Service handles install, login and uninstall without a background worker.
type Service struct {
	store     *storage.DB
	appID     string
	appSecret string
	newClient func(shop, password string) *insales.Client
	now       func() time.Time
}

// NewService creates a lifecycle service.
func NewService(store *storage.DB, appID, appSecret string) *Service {
	return &Service{
		store:     store,
		appID:     appID,
		appSecret: appSecret,
		newClient: func(shop, password string) *insales.Client {
			return insales.NewClient(shop, appID, password)
		},
		now: time.Now,
	}
}

// SetClock replaces the time source (used in tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetClientFactory replaces the InSales client factory.
func (s *Service) SetClientFactory(f func(shop, password string) *insales.Client) {
	s.newClient = f
}

// NewClient creates an API client for a shop.
func (s *Service) NewClient(shop, password string) *insales.Client {
	return s.newClient(shop, password)
}

// InstallParams holds the install callback parameters. Shop is a canonical host.
type InstallParams struct {
	Shop      string
	Token     string
	InsalesID int
}

// InstallResult describes a saved install.
type InstallResult struct {
	Password    string
	CandidateID string
	Generation  int64
	Created     bool
}

// HandleInstall saves a pending install and does not call the InSales API.
func (s *Service) HandleInstall(_ context.Context, params InstallParams) (*InstallResult, error) {
	password := insales.InstallPassword(params.Token, s.appSecret)
	saved, err := s.store.SaveInstall(randomHex(16), params.InsalesID, params.Shop, password)
	if err != nil {
		return nil, fmt.Errorf("save install: %w", err)
	}
	return &InstallResult{
		Password:    password,
		CandidateID: saved.CandidateID,
		Generation:  saved.Generation,
		Created:     saved.Created,
	}, nil
}

// UninstallParams holds the uninstall callback parameters. Shop is a canonical host.
type UninstallParams struct {
	Shop      string
	Token     string
	InsalesID int
}

// HandleUninstall checks the credential and cancels the matching connection.
func (s *Service) HandleUninstall(_ context.Context, params UninstallParams) error {
	storeID := params.InsalesID
	if storeID <= 0 {
		store, err := s.store.GetStoreByShop(params.Shop)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return ErrStoreNotInstalled
			}
			return err
		}
		storeID = store.ID
	}
	removed, err := s.store.Uninstall(storeID, params.Shop, params.Token)
	if errors.Is(err, storage.ErrNotAuthorized) {
		return ErrForbidden
	}
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if !removed {
		return ErrStoreNotInstalled
	}
	return nil
}

// LoginParams holds the initial login request. Shop is a canonical host.
type LoginParams struct {
	Shop      string
	InsalesID int
	UserID    string
}

// LoginResult is the start of a token3 login.
type LoginResult struct {
	ChallengeID string
	Nonce       string
}

// HandleLogin verifies a newer pending install when needed, then starts token3 login.
func (s *Service) HandleLogin(ctx context.Context, params LoginParams, browserID string) (*LoginResult, error) {
	if _, err := s.resolveStore(ctx, params.InsalesID, params.Shop); err != nil {
		return nil, err
	}
	nonce := randomHex(32)
	challengeID := randomHex(16)
	if _, err := s.store.CreateChallenge(challengeID, nonce, browserID, params.InsalesID, ChallengeTTL); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrStoreNotInstalled
		}
		return nil, fmt.Errorf("create challenge: %w", err)
	}
	return &LoginResult{ChallengeID: challengeID, Nonce: nonce}, nil
}

func (s *Service) resolveStore(ctx context.Context, storeID int, shop string) (*storage.Store, error) {
	active, err := s.store.GetActiveStore(storeID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if errors.Is(err, storage.ErrNotFound) {
		active = nil
	}
	if active != nil && active.Shop != shop {
		return nil, ErrForbidden
	}

	pending, err := s.store.NewestPending(storeID, shop)
	if err != nil {
		return nil, err
	}
	usePending := pending != nil && (active == nil || pending.Generation > active.Generation)
	if !usePending {
		if active != nil {
			return active, nil
		}
		other, err := s.store.HasOtherShop(storeID, shop)
		if err != nil {
			return nil, err
		}
		if other {
			return nil, ErrForbidden
		}
		return nil, ErrStoreNotInstalled
	}

	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	client := s.newClient("https://"+pending.Shop, pending.Password)
	account, err := client.GetAccount(verifyCtx)
	if err != nil {
		return s.fallbackAfterVerify(storeID, shop, pending.ID, err)
	}
	if account.ID != pending.StoreID {
		if err := s.store.DeleteCandidate(pending.ID); err != nil {
			return nil, err
		}
		return s.activeOr(storeID, shop, ErrForbidden)
	}

	activated, err := s.store.ActivateCandidate(pending.ID, account.Title)
	if err != nil {
		return nil, err
	}
	if !activated {
		return s.activeOr(storeID, shop, ErrStoreNotInstalled)
	}
	return s.store.GetActiveStore(storeID)
}

func (s *Service) fallbackAfterVerify(storeID int, shop, candidateID string, cause error) (*storage.Store, error) {
	if insales.IsTemporary(cause) {
		return s.activeOr(storeID, shop, fmt.Errorf("%w: %v", ErrTemporary, cause))
	}
	var resource *insales.ResourceError
	if errors.As(cause, &resource) {
		return s.activeOr(storeID, shop, fmt.Errorf("%w: %v", ErrVerification, cause))
	}
	if err := s.store.DeleteCandidate(candidateID); err != nil {
		return nil, err
	}
	return s.activeOr(storeID, shop, ErrForbidden)
}

func (s *Service) activeOr(storeID int, shop string, otherwise error) (*storage.Store, error) {
	active, err := s.store.GetActiveStore(storeID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if active != nil && active.Shop == shop {
		return active, nil
	}
	return nil, otherwise
}

// BuildLoginRedirectURL builds the InSales redirect URL for the login flow.
func BuildLoginRedirectURL(shop, appID, nonce, callbackURL string) string {
	return fmt.Sprintf("https://%s/admin/applications/%s/login?token=%s&login=%s",
		shop, appID, nonce, callbackURL)
}

// AutologinParams holds the InSales login callback.
type AutologinParams struct {
	ChallengeID    string
	Token3         string
	UserEmail      string
	UserName       string
	UserID         string
	EmailConfirmed string
	BrowserID      string
}

// AutologinResult is a created browser session.
type AutologinResult struct {
	SessionID string
	StoreID   int
	UserID    string
}

// HandleAutologin checks token3 and creates a session for the challenge generation.
func (s *Service) HandleAutologin(_ context.Context, params AutologinParams) (*AutologinResult, error) {
	challenge, err := s.store.ConsumeChallenge(params.ChallengeID, params.BrowserID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrExpired
		}
		if errors.Is(err, storage.ErrNotAuthorized) || errors.Is(err, storage.ErrStale) {
			return nil, ErrForbidden
		}
		return nil, fmt.Errorf("consume challenge: %w", err)
	}

	store, err := s.store.GetActiveStore(challenge.StoreID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrStoreNotInstalled
		}
		return nil, err
	}
	if store.Generation != challenge.Generation {
		return nil, ErrForbidden
	}

	expected := insales.Token3(
		challenge.Nonce,
		params.UserEmail,
		params.UserName,
		params.UserID,
		params.EmailConfirmed,
		store.Password,
	)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(params.Token3)) != 1 {
		return nil, ErrInvalidSignature
	}

	sessionID := randomHex(32)
	if err := s.store.CreateSession(sessionID, challenge.StoreID, params.UserID, challenge.Generation, SessionTTL); err != nil {
		if errors.Is(err, storage.ErrStale) {
			return nil, ErrForbidden
		}
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &AutologinResult{SessionID: sessionID, StoreID: challenge.StoreID, UserID: params.UserID}, nil
}

// VerifySession checks a session together with its current connection.
func (s *Service) VerifySession(sessionID string) (*storage.Session, *storage.Store, error) {
	sess, store, err := s.store.VerifySession(sessionID)
	if err != nil {
		return nil, nil, err
	}
	return sess, store, nil
}

// Logout terminates a session.
func (s *Service) Logout(sessionID string) error {
	return s.store.DeleteSession(sessionID)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate random: %v", err))
	}
	return hex.EncodeToString(b)
}
