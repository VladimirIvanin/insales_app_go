// Package httpapp provides the HTTP server, routes, middleware and templates
// for the InSales partner application.
package httpapp

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/config"
	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

//go:embed templates/*.html
var templateFS embed.FS

// App is the HTTP application container.
type App struct {
	cfg       *config.Config
	lifecycle *lifecycle.Service
	store     interface{ Close() error }
	storeDB   *storage.DB
	tmpls     *template.Template
	server    *http.Server
	verifyCh  chan struct{} // signal to re-check candidates
	isSecure  bool          // whether cookies should have Secure flag
}

// DashboardData represents data for the dashboard template.
type DashboardData struct {
	Title       string
	Shop        string
	StoreID     int
	Title2      string
	UserID      string
	ProbeResult string
	ProbeError  bool
}

// ErrorData represents data for the error template.
type ErrorData struct {
	Message string
}

// New creates a new HTTP application with routes and middleware.
func New(cfg *config.Config, lc *lifecycle.Service, storeDB *storage.DB) (*App, error) {
	tmpls, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	verifyCh := make(chan struct{}, 1)

	app := &App{
		cfg:       cfg,
		lifecycle: lc,
		store:     storeDB,
		storeDB:   storeDB,
		tmpls:     tmpls,
		verifyCh:  verifyCh,
		isSecure:  strings.HasPrefix(cfg.BaseURL, "https://"),
	}

	// Start background candidate verification worker
	go app.verificationWorker()

	mux := http.NewServeMux()
	mux.HandleFunc("/install", app.handleInstall)
	mux.HandleFunc("/login", app.handleLogin)
	mux.HandleFunc("/auth/callback", app.handleAuthCallback)
	mux.HandleFunc("/logout", app.handleLogout)
	mux.HandleFunc("/uninstall", app.handleUninstall)
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/probe", app.handleProbe)
	mux.HandleFunc("/", app.handleDashboard)

	// Wrap with CSRF protection middleware for state-changing endpoints
	handler := csrfMiddleware(sessionMiddleware(app, mux))

	app.server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.BindHost, cfg.Port),
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	return app, nil
}

// ListenAndServe starts the HTTP server.
func (a *App) ListenAndServe() error {
	slog.Info("starting server", "addr", a.server.Addr)
	return a.server.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (a *App) Shutdown(ctx context.Context) error {
	return a.server.Shutdown(ctx)
}

// Handler returns the configured HTTP handler with all middleware applied.
// This is used in tests to build test servers via httptest.NewServer.
func (a *App) Handler() http.Handler {
	return a.server.Handler
}

// verificationWorker processes candidate verifications in background.
// Reads candidates from the storage queue, retries on failure with backoff
// managed by ClaimCandidate/FailCandidate, and periodically polls for
// candidates whose next_attempt_at has elapsed.
func (a *App) verificationWorker() {
	ctx := context.Background()

	poll := func() {
		for {
			candidate, err := a.storeDB.ClaimCandidate()
			if err != nil {
				slog.Error("claim candidate", "error", err)
				return
			}
			if candidate == nil {
				return // no candidates available
			}

			slog.Info("verifying install candidate", "store", candidate.StoreID, "shop", candidate.Shop, "attempt", candidate.Attempts)

			err = a.lifecycle.VerifyInstall(ctx, candidate)
			if err != nil {
				// Determine retry delay from error type
				retryAfter := 30
				if insales.IsTransientError(err) {
					retryAfter = 10
				}
				var apiErr *insales.APIError
				if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
					retryAfter = apiErr.RetryAfter
				}

				if failErr := a.storeDB.FailCandidate(candidate.ID, err.Error(), retryAfter); failErr != nil {
					slog.Error("fail candidate", "candidate", candidate.ID, "error", failErr)
				}
				slog.Warn("verification failed, will retry", "store", candidate.StoreID, "retryAfter", retryAfter, "error", err)
				continue
			}

			slog.Info("verification succeeded", "store", candidate.StoreID, "shop", candidate.Shop)
		}
	}

	// Initial poll on startup (catches candidates from before restart)
	poll()

	// Periodic poll — catches candidates whose next_attempt_at has elapsed
	// even when no new install signal arrives.
	pollTicker := time.NewTicker(30 * time.Second)
	defer pollTicker.Stop()

	// Periodic cleanup of expired sessions and challenges (every 5 minutes)
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-a.verifyCh:
			poll()
		case <-pollTicker.C:
			poll()
		case <-cleanupTicker.C:
			a.storeDB.CleanupExpired()
		}
	}
}

// sessionMiddleware extracts the session cookie and injects it into the request context.
func sessionMiddleware(app *App, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip session check for public routes
		publicRoutes := map[string]bool{
			"/install":   true,
			"/login":     true,
			"/uninstall": true,
			"/healthz":   true,
		}
		if publicRoutes[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		// For session-protected routes, check cookie
		if r.URL.Path == "/auth/callback" {
			// Auth callback is special — it should have a challenge cookie
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// csrfMiddleware protects POST endpoints from CSRF.
// Uses SameSite=Lax cookie as primary protection.
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// getSessionID extracts the session ID from the cookie.
func getSessionID(r *http.Request) string {
	cookie, err := r.Cookie("session_id")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// setSessionCookie sets the session cookie on the response.
func (a *App) setSessionCookie(w http.ResponseWriter, sessionID string) {
	// #nosec G124 -- Secure повторяет схему APP_BASE_URL: для loopback HTTP cookie без Secure
	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   3600, // 1 hour
	})
}

// clearSessionCookie removes the session cookie.
func (a *App) clearSessionCookie(w http.ResponseWriter) {
	// #nosec G124 -- Secure повторяет схему APP_BASE_URL: для loopback HTTP cookie без Secure
	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// setChallengeCookie sets the challenge cookie for login binding.
func (a *App) setChallengeCookie(w http.ResponseWriter, challengeID string) {
	// #nosec G124 -- Secure повторяет схему APP_BASE_URL: для loopback HTTP cookie без Secure
	http.SetCookie(w, &http.Cookie{
		Name:     "challenge_id",
		Value:    challengeID,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 minutes
	})
}

// getChallengeID extracts challenge ID from cookie.
func getChallengeID(r *http.Request) string {
	cookie, err := r.Cookie("challenge_id")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// clearChallengeCookie removes the challenge cookie.
func (a *App) clearChallengeCookie(w http.ResponseWriter) {
	// #nosec G124 -- Secure повторяет схему APP_BASE_URL: для loopback HTTP cookie без Secure
	http.SetCookie(w, &http.Cookie{
		Name:     "challenge_id",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// getOrCreateBrowserID generates or retrieves a browser fingerprint cookie.
func (a *App) getOrCreateBrowserID(r *http.Request, w http.ResponseWriter) string {
	cookie, err := r.Cookie("browser_id")
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate browser ID: %v", err))
	}
	browserID := hex.EncodeToString(b)

	// #nosec G124 -- Secure повторяет схему APP_BASE_URL: для loopback HTTP cookie без Secure
	http.SetCookie(w, &http.Cookie{
		Name:     "browser_id",
		Value:    browserID,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400, // 24 hours
	})

	return browserID
}

// --- Handlers ---

// handleInstall processes GET /install?shop=...&token=...&insales_id=...
func (a *App) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	shop := r.URL.Query().Get("shop")
	token := r.URL.Query().Get("token")
	insalesIDStr := r.URL.Query().Get("insales_id")

	if shop == "" || token == "" || insalesIDStr == "" {
		writeError(w, a.tmpls, "Неверные параметры установки: shop, token и insales_id обязательны", http.StatusBadRequest)
		return
	}

	// Validate insales_id is numeric
	var insalesID int
	if id, err := strconv.Atoi(insalesIDStr); err != nil || id <= 0 {
		writeError(w, a.tmpls, "Неверный insales_id", http.StatusBadRequest)
		return
	} else {
		insalesID = id
	}

	// Check for duplicate query parameters (security)
	if len(r.URL.Query()["shop"]) != 1 || len(r.URL.Query()["token"]) != 1 || len(r.URL.Query()["insales_id"]) != 1 {
		writeError(w, a.tmpls, "Повторяющиеся параметры запроса", http.StatusBadRequest)
		return
	}

	// Validate and normalize shop URL
	shopURL, err := insales.ValidateShopURL(shop, a.cfg.AllowedShops)
	if err != nil {
		slog.Warn("install: invalid shop", "shop", shop, "error", err)
		writeError(w, a.tmpls, "Магазин не найден в списке разрешённых", http.StatusForbidden)
		return
	}

	// Remove https:// prefix for storage
	shop = strings.TrimPrefix(shopURL, "https://")

	params := lifecycle.InstallParams{
		Shop:      shop,
		Token:     token,
		InsalesID: insalesID,
	}

	result, err := a.lifecycle.HandleInstall(r.Context(), params)
	if err != nil {
		slog.Error("install failed", "shop", shop, "error", err)
		writeError(w, a.tmpls, "Ошибка установки", http.StatusInternalServerError)
		return
	}

	// Signal background verification
	select {
	case a.verifyCh <- struct{}{}:
	default:
		slog.Warn("verification worker busy, candidate will be processed on next poll cycle")
	}

	slog.Info("install callback processed", "shop", shop, "insales_id", insalesID, "candidate", result.CandidateID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpls.ExecuteTemplate(w, "install_ok.html", nil); err != nil {
		slog.Error("render install page", "error", err)
	}
}

// handleLogin processes GET /login?shop=...&insales_id=...&user_id=...
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	shop := r.URL.Query().Get("shop")
	insalesIDStr := r.URL.Query().Get("insales_id")
	userID := r.URL.Query().Get("user_id")

	if shop == "" || insalesIDStr == "" {
		writeError(w, a.tmpls, "Неверные параметры входа", http.StatusBadRequest)
		return
	}

	var insalesID int
	if id, err := strconv.Atoi(insalesIDStr); err != nil || id <= 0 {
		writeError(w, a.tmpls, "Неверный insales_id", http.StatusBadRequest)
		return
	} else {
		insalesID = id
	}

	// Get or create browser binding
	browserID := a.getOrCreateBrowserID(r, w)

	result, err := a.lifecycle.HandleLogin(lifecycle.LoginParams{
		Shop:      shop,
		InsalesID: insalesID,
		UserID:    userID,
	}, browserID)
	if err != nil {
		if err == lifecycle.ErrStoreNotInstalled {
			writeError(w, a.tmpls, "Магазин не установлен", http.StatusNotFound)
			return
		}
		slog.Error("login failed", "shop", shop, "error", err)
		writeError(w, a.tmpls, "Ошибка входа", http.StatusInternalServerError)
		return
	}

	// Set challenge cookie for callback binding
	a.setChallengeCookie(w, result.ChallengeID)

	// Build redirect URL to InSales with the nonce
	nonce := result.Nonce
	redirectURL := lifecycle.BuildLoginRedirectURL(shop, a.cfg.AppID, nonce, a.cfg.BaseURL+"/auth/callback")

	slog.Info("login initiated", "shop", shop, "challenge", result.ChallengeID, "nonce", nonce[:8]+"...")

	http.Redirect(w, r, redirectURL, http.StatusFound) // #nosec G710 -- host совпадает с shop установленного магазина
}

// handleAuthCallback processes GET /auth/callback after InSales login redirect.
// InSales redirects to: /auth/callback?token=...&token2=...&token3=...&user_email=...&user_name=...&user_id=...&email_confirmed=...
// Note: The actual callback URL configured in InSales matters.
// Per InSales docs, the callback URL format is different — InSales redirects with
// token, token2, token3 as path params or query params depending on configuration.
// We handle the query param format here.
func (a *App) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token3 := r.URL.Query().Get("token3")
	userEmail := r.URL.Query().Get("user_email")
	userName := r.URL.Query().Get("user_name")
	userID := r.URL.Query().Get("user_id")
	emailConfirmed := r.URL.Query().Get("email_confirmed")

	if token3 == "" || userEmail == "" || userID == "" {
		writeError(w, a.tmpls, "Неверные параметры callback", http.StatusBadRequest)
		return
	}

	challengeID := getChallengeID(r)
	if challengeID == "" {
		writeError(w, a.tmpls, "Нет активного запроса на вход", http.StatusUnauthorized)
		return
	}

	browserID := a.getOrCreateBrowserID(r, w)

	a.clearChallengeCookie(w)

	result, err := a.lifecycle.HandleAutologin(r.Context(), lifecycle.AutologinParams{
		ChallengeID:    challengeID,
		Token3:         token3,
		UserEmail:      userEmail,
		UserName:       userName,
		UserID:         userID,
		EmailConfirmed: emailConfirmed,
		BrowserID:      browserID,
	})
	if err != nil {
		if err == lifecycle.ErrExpired {
			writeError(w, a.tmpls, "Срок действия запроса на вход истёк", http.StatusUnauthorized)
			return
		}
		if err == lifecycle.ErrInvalidSignature {
			writeError(w, a.tmpls, "Неверная подпись входа", http.StatusForbidden)
			return
		}
		if err == lifecycle.ErrForbidden {
			writeError(w, a.tmpls, "Доступ запрещён", http.StatusForbidden)
			return
		}
		slog.Error("autologin failed", "error", err)
		writeError(w, a.tmpls, "Ошибка входа", http.StatusInternalServerError)
		return
	}

	// Set session cookie
	a.setSessionCookie(w, result.SessionID)

	slog.Info("user logged in", "store", result.StoreID, "user", result.UserID)

	// Redirect to dashboard
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDashboard processes GET / — shows the main app page.
func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	sessionID := getSessionID(r)
	if sessionID == "" {
		writeError(w, a.tmpls, "Необходимо войти в приложение", http.StatusUnauthorized)
		return
	}

	sess, store, err := a.lifecycle.VerifySession(sessionID)
	if err != nil {
		a.clearSessionCookie(w)
		writeError(w, a.tmpls, "Сессия истекла. Войдите снова.", http.StatusUnauthorized)
		return
	}

	data := DashboardData{
		Title:   store.Title,
		Shop:    store.Shop,
		StoreID: store.ID,
		Title2:  store.Title,
		UserID:  sess.UserID,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpls.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		slog.Error("render dashboard", "error", err)
	}
}

// handleProbe processes POST /probe — makes an API call to verify connectivity.
func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := getSessionID(r)
	if sessionID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sess, store, err := a.lifecycle.VerifySession(sessionID)
	if err != nil {
		a.clearSessionCookie(w)
		http.Error(w, "Session expired", http.StatusUnauthorized)
		return
	}

	client := a.lifecycle.NewClient("https://"+store.Shop, store.Password)
	account, err := client.GetAccount(r.Context())
	if err != nil {
		data := DashboardData{
			Title:       store.Title,
			Shop:        store.Shop,
			StoreID:     store.ID,
			UserID:      sess.UserID,
			ProbeResult: fmt.Sprintf("Ошибка API: %s", html.EscapeString(err.Error())),
			ProbeError:  true,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := a.tmpls.ExecuteTemplate(w, "dashboard.html", data); err != nil {
			slog.Error("render dashboard", "error", err)
		}
		return
	}

	data := DashboardData{
		Title:       store.Title,
		Shop:        store.Shop,
		StoreID:     store.ID,
		UserID:      sess.UserID,
		ProbeResult: fmt.Sprintf("API доступен. Магазин: %s (ID: %d)", html.EscapeString(account.Title), account.ID),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpls.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		slog.Error("render dashboard", "error", err)
	}
}

// handleLogout processes POST /logout — terminates a session.
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := getSessionID(r)
	if sessionID != "" {
		if err := a.lifecycle.Logout(sessionID); err != nil {
			slog.Error("logout failed", "error", err)
		}
	}

	a.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleUninstall processes GET /uninstall?shop=...&token=...&insales_id=...
func (a *App) handleUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	shop := r.URL.Query().Get("shop")
	token := r.URL.Query().Get("token")
	insalesIDStr := r.URL.Query().Get("insales_id")

	if shop == "" || token == "" {
		writeError(w, a.tmpls, "Неверные параметры удаления", http.StatusBadRequest)
		return
	}

	// Check for duplicate query parameters
	if len(r.URL.Query()["shop"]) != 1 || len(r.URL.Query()["token"]) != 1 {
		writeError(w, a.tmpls, "Повторяющиеся параметры запроса", http.StatusBadRequest)
		return
	}

	var insalesID int
	if insalesIDStr != "" {
		id, err := strconv.Atoi(insalesIDStr)
		if err != nil {
			writeError(w, a.tmpls, "Неверный insales_id", http.StatusBadRequest)
			return
		}
		insalesID = id
	}

	err := a.lifecycle.HandleUninstall(r.Context(), lifecycle.UninstallParams{
		Shop:      shop,
		Token:     token,
		InsalesID: insalesID,
	})
	if err != nil && err != lifecycle.ErrStoreNotInstalled && err != lifecycle.ErrForbidden {
		slog.Error("uninstall error", "shop", shop, "error", err)
		// Return 500 so InSales will retry the callback
		writeError(w, a.tmpls, "Ошибка удаления", http.StatusInternalServerError)
		return
	}
	if err == lifecycle.ErrForbidden {
		slog.Warn("uninstall: wrong password", "shop", shop)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		if _, err := w.Write([]byte("Forbidden")); err != nil {
			slog.Debug("write uninstall response", "error", err)
		}
		return
	}

	slog.Info("uninstall processed", "shop", shop, "insales_id", insalesID)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("OK")); err != nil {
		slog.Debug("write uninstall response", "error", err)
	}
}

// handleHealthz processes GET /healthz.
func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	if _, err := w.Write([]byte("OK")); err != nil {
		slog.Debug("write health response", "error", err)
	}
}

// writeError renders the error template with the given message and status code.
func writeError(w http.ResponseWriter, tmpls *template.Template, message string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpls.ExecuteTemplate(w, "error.html", ErrorData{Message: message}); err != nil {
		slog.Error("render error page", "error", err)
	}
}
