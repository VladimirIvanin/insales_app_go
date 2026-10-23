# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 1.x     | :white_check_mark: |

## Reporting a Vulnerability

If you discover a security vulnerability in this project, please report it privately via
[GitHub Security Advisory](https://github.com/ivaninvladimir/insales_app_go/security/advisories/new).
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

3. **HTTP-Only Cookies**: Session cookies are HTTP-only and use SameSite=Lax. The `Secure` flag follows the `APP_BASE_URL` scheme: it is set for HTTPS and omitted for HTTP. Publishing the application over HTTPS is the operator's responsibility. `BIND_HOST` defaults to `127.0.0.1`.

4. **CSRF Protection**: State-changing POST endpoints are protected by `SameSite=Lax` cookie attribute. No additional Origin/Referer header checks are performed — the SameSite attribute is the sole CSRF defence.

5. **Input Validation**:
   - Shop URLs are parsed with `net/url.Parse` and validated against the parsed `Hostname` only. Userinfo, path, fragment, and query parameters are rejected.
   - Duplicate `shop`, `token` and `insales_id` query parameters on install and uninstall are rejected. The first value is not used.
   - Shop domain **must** match an explicit allowlist. An empty `ALLOWED_SHOPS` is **refused by default** — at least one domain must be configured. This prevents an attacker from pointing their own HTTPS server at install/login/uninstall endpoints and passing `account.id` checks with a synthetic InSales-compatible response.
   - IP addresses are rejected as shop domains.
   - `insales_id` is parsed with `strconv.Atoi` (full-string, not partial).

6. **No Secrets in Logs**: Passwords, tokens, and full callback query strings are not logged.

7. **API Client Safety**: Redirects are disabled in the API client; response body size is limited to 1 MB; timeouts and strict transport settings are configured. Each `Client` creates one transport and reuses it for its own requests. Install verification, `/probe` and `doctor` each construct a new client.

8. **Shop API and application URL**: Calls from the application to a shop use `https://` and the canonical host. `NewClient` itself accepts the URL it is given; tests point it at the HTTP simulator. `APP_BASE_URL` may be HTTP or HTTPS. An HTTP application URL leaves the session cookie without `Secure`. Serving the application over HTTPS is the operator's responsibility.

9. **Dependency Scanning**: GitHub Actions run `govulncheck` and `gosec` on every push and PR.

10. **Dependabot**: Automated dependency updates for Go modules and GitHub Actions.

11. **SQLite Hardening**:
    - WAL journal mode with `busy_timeout=5000` set via `PRAGMA` after opening the connection.
    - `SetMaxOpenConns(1)` serialises all transactions within a single process, preventing concurrent write conflicts.
    - `PRAGMA foreign_keys = ON` is set on the connection, not per-transaction.

12. **Lifecycle Safety**:
    - The installation generation is reserved in the same write transaction as the candidate and is not reset on uninstall.
    - `/install` only stores a candidate. `account.json` is checked on the next `/login`, with a five-second limit and without a background queue.
    - A temporary API error keeps the candidate. The user retries by opening the app again.
    - Activation re-reads the candidate and does not restore a row that was removed during the request.
    - A database error on uninstall returns HTTP 500 so InSales retries the callback.
    - Uninstall checks the credential in one transaction. A matching active connection removes that connection, its sessions and challenges, and pending rows with the same password. A pending install with a different password is kept, including when its callback arrived before a delayed uninstall of the previous connection. A matching pending password does not remove an active connection or other pending passwords.
    - Challenges and sessions are inserted only for the generation that is still current. An old login cannot read credentials of a newer installation.
    - At most five unexpired pending installs are kept per shop domain, for seven days. A new password does not delete an already stored candidate. When five candidates are still unexpired, `/install` responds with HTTP 503 and stores nothing. After expiry, the next install is stored again.
    - Expired challenges and sessions are deleted when a new one is created. TTL is checked on every read.

## Scope

- This policy covers the `insales_app_go` repository only.
- MD5 is used exclusively for InSales protocol compatibility (password and token3 signatures). It is not used for any other security purpose.
- SQLite is used for local storage. For production deployments, additional database hardening may be required.