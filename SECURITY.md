# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 1.x     | :white_check_mark: |

## Reporting a Vulnerability

If you discover a security vulnerability in this project, please report it by email to the maintainer.
Do NOT open a public GitHub issue for security vulnerabilities.

Please include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Any suggested fixes (if known)

We will acknowledge receipt within 48 hours and provide a timeline for resolution.

## Security Measures

This project implements the following security measures:

1. **Password Encryption**: Store passwords are encrypted at rest using AES-256-GCM authenticated encryption.

2. **Constant-Time Comparisons**: Token3 verification uses `crypto/subtle.ConstantTimeCompare`. Uninstall password verification also uses constant-time comparison.

3. **HTTP-Only Cookies**: Session cookies are HTTP-only and use SameSite=Lax. The `Secure` flag is set dynamically based on the application URL scheme (enabled for HTTPS, disabled for HTTP loopback).

4. **CSRF Protection**: State-changing POST endpoints are protected by `SameSite=Lax` cookie attribute. No additional Origin/Referer header checks are performed — the SameSite attribute is the sole CSRF defence.

5. **Input Validation**:
   - Shop URLs are parsed with `net/url.Parse` and validated against the parsed `Hostname` only. Userinfo, path, fragment, and query parameters are rejected.
   - Duplicate query parameters on install/uninstall are rejected.
   - Shop domain **must** match an explicit allowlist. An empty `ALLOWED_SHOPS` is **refused by default** — at least one domain must be configured. This prevents an attacker from pointing their own HTTPS server at install/login/uninstall endpoints and passing `account.id` checks with a synthetic InSales-compatible response.
   - IP addresses are rejected as shop domains.
   - `insales_id` is parsed with `strconv.Atoi` (full-string, not partial).

6. **No Secrets in Logs**: Passwords, tokens, and full callback query strings are not logged.

7. **API Client Safety**: Redirects are disabled in the API client; response body size is limited to 1 MB; timeouts and strict transport settings are configured. The HTTP client transport is created once and reused.

8. **TLS Required**: Production shops use HTTPS. HTTP is only allowed for local test simulators and explicit `APP_BASE_URL=http://127.0.0.1` development configuration.

9. **Dependency Scanning**: GitHub Actions run `govulncheck` and `gosec` on every push and PR.

10. **Dependabot**: Automated dependency updates for Go modules and GitHub Actions.

11. **SQLite Hardening**:
    - WAL journal mode with `busy_timeout=5000` set via `PRAGMA` after opening the connection.
    - `SetMaxOpenConns(1)` serialises all transactions within a single process, preventing concurrent write conflicts.
    - `PRAGMA foreign_keys = ON` is set on the connection, not per-transaction.

12. **Lifecycle Safety**:
    - Candidate generation is reserved atomically within a transaction.
    - Background verification worker runs periodic polls (every 30s) and processes on-demand signals, ensuring candidates whose `next_attempt_at` has elapsed are retried even without new install events.
    - Expired sessions and challenges are cleaned up every 5 minutes by the background worker.
    - `ClaimCandidate` uses exponential backoff (10s, 30s, 90s) and unique lease tokens.
    - Transient API errors (429, 5xx, transport) are recorded with `Retry-After` header value and re-scheduled.
    - Uninstall with a database error returns HTTP 500 so InSales retries the callback.
    - Uninstall verifies credentials inside a single write-transaction (TOCTOU prevention). Full store deletion only happens when the active store credential matches; a matching candidate password removes only that candidate.
    - Session creation and verification include `installation_epoch`, a monotonic counter that increments on every reinstall. This prevents a session captured during a previous installation from being reused after uninstall+reinstall.
    - Per-store candidate limit (max 5) and global candidate limit (max 100) prevent backlog exhaustion.
    - Expired challenge cleanup is committed to the database (not rolled back).

## Scope

- This policy covers the `insales_app_go` repository only.
- MD5 is used exclusively for InSales protocol compatibility (password and token3 signatures). It is not used for any other security purpose.
- SQLite is used for local storage. For production deployments, additional database hardening may be required.