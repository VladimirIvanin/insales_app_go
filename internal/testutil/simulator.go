// Package testutil provides utilities for testing the InSales application,
// including a lightweight InSales platform simulator.
package testutil

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Required for InSales protocol compatibility in simulator
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Response represents a simulated platform response.
type Response struct {
	Status     int
	Body       string
	Headers    map[string]string
	Disconnect bool          // if true, close connection without sending response
	Wait       chan struct{} // if set, block until signalled (for race testing)
}

// PlatformSimulator simulates the InSales platform for lifecycle testing.
type PlatformSimulator struct {
	mu         sync.Mutex
	server     *http.Server
	listener   net.Listener
	port       int
	shopDomain string

	appID       string
	appSecret   string
	installURL  string
	callbackURL string

	passwords     map[int]string       // store_id -> password
	accounts      map[int]accountInfo  // store_id -> account info
	routes        map[string]*Response // method+path -> canned response
	nextResponses []routeOverride

	storeStates map[int]storeState

	rateLimitWindow time.Duration
	rateLimitCount  int
	requestLog      []time.Time
	limitsPerStore  map[int]*rateLimitState

	closed atomic.Bool
}

type accountInfo struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

type storeState struct {
	Installed bool
	Password  string
	AppID     string
}

type routeOverride struct {
	method string
	path   string
	resp   Response
}

type rateLimitState struct {
	window []time.Time
	mu     sync.Mutex
}

// NewPlatformSimulator creates a new platform simulator.
// It listens on a random loopback port.
func NewPlatformSimulator(appID, appSecret, installURL, callbackURL string) (*PlatformSimulator, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	ps := &PlatformSimulator{
		listener:        listener,
		port:            listener.Addr().(*net.TCPAddr).Port,
		appID:           appID,
		appSecret:       appSecret,
		installURL:      installURL,
		callbackURL:     callbackURL,
		passwords:       make(map[int]string),
		accounts:        make(map[int]accountInfo),
		routes:          make(map[string]*Response),
		storeStates:     make(map[int]storeState),
		rateLimitWindow: 300 * time.Second,
		rateLimitCount:  100,
		limitsPerStore:  make(map[int]*rateLimitState),
		shopDomain:      fmt.Sprintf("127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port),
	}

	ps.server = &http.Server{
		Handler: http.HandlerFunc(ps.handle),
	}

	go ps.server.Serve(listener)

	return ps, nil
}

// Close shuts down the simulator.
func (ps *PlatformSimulator) Close() {
	ps.closed.Store(true)
	ps.server.Close()
}

// Port returns the listening port.
func (ps *PlatformSimulator) Port() int { return ps.port }

// ShopDomain returns the shop domain pointing to the simulator.
func (ps *PlatformSimulator) ShopDomain() string { return ps.shopDomain }

// ShopURL returns the full shop URL for API calls.
func (ps *PlatformSimulator) ShopURL() string {
	return fmt.Sprintf("http://%s", ps.shopDomain)
}

// InstallPassword computes the password for a given token.
func (ps *PlatformSimulator) InstallPassword(token string) string {
	h := md5.New() //nolint:gosec // Required for InSales protocol compatibility
	h.Write([]byte(token))
	h.Write([]byte(ps.appSecret))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// SetAccount configures an account for a given store ID.
func (ps *PlatformSimulator) SetAccount(storeID int, title string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.accounts[storeID] = accountInfo{ID: storeID, Title: title}
}

// ConfigureStore sets up a store with a password.
func (ps *PlatformSimulator) ConfigureStore(storeID int, password string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.passwords[storeID] = password
	ps.storeStates[storeID] = storeState{
		Installed: true,
		Password:  password,
		AppID:     ps.appID,
	}
}

// RevokeStore removes a store's password (after uninstall).
func (ps *PlatformSimulator) RevokeStore(storeID int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.passwords, storeID)
	ps.storeStates[storeID] = storeState{Installed: false}
}

// IsInstalled checks if a store is installed.
func (ps *PlatformSimulator) IsInstalled(storeID int) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.storeStates[storeID].Installed
}

// Define sets a canned response for a given method+path.
func (ps *PlatformSimulator) Define(method, path string, resp Response) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.routes[method+" "+path] = &resp
}

// RespondNext sets a one-time override for the next matching request.
func (ps *PlatformSimulator) RespondNext(method, path string, resp Response) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.nextResponses = append(ps.nextResponses, routeOverride{method, path, resp})
}

// Install sends an install callback to the application.
func (ps *PlatformSimulator) Install(appInstallURL, shop, token string) (int, error) {
	u := fmt.Sprintf("%s?shop=%s&token=%s&insales_id=42", appInstallURL, shop, token)
	resp, err := http.Get(u)
	if err != nil {
		return 0, fmt.Errorf("install request: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Uninstall sends an uninstall callback to the application.
func (ps *PlatformSimulator) Uninstall(appUninstallURL, shop, password string) (int, error) {
	u := fmt.Sprintf("%s?shop=%s&token=%s&insales_id=42", appUninstallURL, shop, password)
	resp, err := http.Get(u)
	if err != nil {
		return 0, fmt.Errorf("uninstall request: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Login initiates a login flow by redirecting the browser to the app.
func (ps *PlatformSimulator) Login(appLoginURL, shop, appID string) (redirectURL string, err error) {
	u := fmt.Sprintf("%s?shop=%s&insales_id=42&user_id=1", appLoginURL, shop)
	resp, err := http.Get(u)
	if err != nil {
		return "", fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	// The app should either redirect or return a page with redirect script
	// We extract the redirect URL from the response
	_ = resp
	return "", nil
}

// CompleteLogin simulates the InSales login callback by computing signatures
// and calling the app's callback URL.
func (ps *PlatformSimulator) CompleteLogin(callbackURL, nonce, password string,
	email, name, userID, emailConfirmed string) (*http.Response, error) {

	// MD5 signatures calculated as per InSales protocol
	token3 := ps.computeToken3(nonce, email, name, userID, emailConfirmed, password)

	callbackURL = strings.TrimRight(callbackURL, "/?")
	fullURL := fmt.Sprintf("%s?token3=%s&user_email=%s&user_name=%s&user_id=%s&email_confirmed=%s",
		callbackURL, token3, url.QueryEscape(email), url.QueryEscape(name), userID, emailConfirmed)

	return http.Get(fullURL)
}

func (ps *PlatformSimulator) computeToken3(nonce, email, name, userID, emailConfirmed, password string) string {
	h := md5.New() //nolint:gosec // Required for InSales protocol compatibility
	h.Write([]byte(nonce))
	h.Write([]byte(email))
	h.Write([]byte(name))
	h.Write([]byte(userID))
	h.Write([]byte(emailConfirmed))
	h.Write([]byte(password))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// handle routes incoming requests to the simulator.
func (ps *PlatformSimulator) handle(w http.ResponseWriter, r *http.Request) {
	// Check canned routes first
	key := r.Method + " " + r.URL.Path
	ps.mu.Lock()
	// Check one-time overrides
	for i, ov := range ps.nextResponses {
		if ov.method == r.Method && ov.path == r.URL.Path {
			ps.nextResponses = append(ps.nextResponses[:i], ps.nextResponses[i+1:]...)
			ps.mu.Unlock()
			ps.sendResponse(w, ov.resp)
			return
		}
	}
	if resp, ok := ps.routes[key]; ok {
		ps.mu.Unlock()
		ps.sendResponse(w, *resp)
		return
	}
	ps.mu.Unlock()

	switch {
	case r.URL.Path == "/admin/account.json":
		ps.handleAccount(w, r)
	case strings.Contains(r.URL.Path, "/admin/applications/") && strings.HasSuffix(r.URL.Path, "/login"):
		ps.handleLoginRedirect(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleAccount handles GET /admin/account.json with Basic Auth.
func (ps *PlatformSimulator) handleAccount(w http.ResponseWriter, r *http.Request) {
	// Check Basic Auth
	user, pass, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="InSales"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Verify credentials
	if user != ps.appID {
		http.Error(w, `{"error":"invalid application"}`, http.StatusForbidden)
		return
	}

	// Find store by password
	ps.mu.Lock()
	var foundStore int
	found := false
	for id, pw := range ps.passwords {
		if pw == pass {
			foundStore = id
			found = true
			break
		}
	}
	acc, hasAccount := ps.accounts[foundStore]
	ps.mu.Unlock()

	if !found {
		http.Error(w, `{"error":"invalid password"}`, http.StatusUnauthorized)
		return
	}

	if !hasAccount {
		// Return basic account with id from password
		acc = accountInfo{ID: foundStore, Title: fmt.Sprintf("Shop %d", foundStore)}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(acc)
}

// handleLoginRedirect handles the InSales login redirect.
func (ps *PlatformSimulator) handleLoginRedirect(w http.ResponseWriter, r *http.Request) {
	// Parse the application login redirect
	token := r.URL.Query().Get("token")
	loginURL := r.URL.Query().Get("login")

	if token == "" || loginURL == "" {
		http.Error(w, "Missing token or login URL", http.StatusBadRequest)
		return
	}

	// Simulate the InSales login callback by computing signatures
	// and redirecting the browser to the application's callback URL

	// We would need the password to compute token3
	// The password is stored in the simulator
	// For now, just acknowledge the redirect
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Login simulated"))
}

// sendResponse sends a canned response.
func (ps *PlatformSimulator) sendResponse(w http.ResponseWriter, resp Response) {
	if resp.Disconnect {
		// Hijack and close the connection
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
		return
	}

	if resp.Wait != nil {
		<-resp.Wait
	}

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.Status)
	w.Write([]byte(resp.Body))
}

// Request is a helper to make requests to the simulator.
func (ps *PlatformSimulator) Request(method, path string, body []byte, headers map[string]string) (*http.Response, error) {
	u := fmt.Sprintf("http://%s%s", ps.shopDomain, path)
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, reqBody)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// BasicAuthHeader returns the Authorization header value for Basic auth.
func BasicAuthHeader(appID, password string) string {
	auth := appID + ":" + password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
}
