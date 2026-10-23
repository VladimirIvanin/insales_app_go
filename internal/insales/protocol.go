// Package insales implements InSales partner application protocol formulas.
//
// MD5 is used here exclusively for compatibility with the InSales platform
// protocol (install password, token, token2, token3 signatures).
// Per InSales specification: password = MD5(token + app_secret),
// token3 = MD5(nonce + email + name + user_id + email_confirmed + password).
package insales

import (
	"crypto/md5" // #nosec G501 -- Required for InSales protocol compatibility
	"fmt"
)

// InstallPassword computes the InSales store password.
// Formula: MD5(token + app_secret) as lowercase hex.
func InstallPassword(token, appSecret string) string {
	h := md5.New() // #nosec G401 -- Required for InSales protocol compatibility
	h.Write([]byte(token))
	h.Write([]byte(appSecret))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Token computes the original InSales autologin token.
// Formula: MD5(nonce + password) as lowercase hex.
func Token(nonce, password string) string {
	h := md5.New() // #nosec G401 -- Required for InSales protocol compatibility
	h.Write([]byte(nonce))
	h.Write([]byte(password))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Token2 computes the second-generation InSales autologin token.
// Formula: MD5(nonce + user_email + user_name + user_id + password) as lowercase hex.
func Token2(nonce, userEmail, userName, userID, password string) string {
	h := md5.New() // #nosec G401 -- Required for InSales protocol compatibility
	h.Write([]byte(nonce))
	h.Write([]byte(userEmail))
	h.Write([]byte(userName))
	h.Write([]byte(userID))
	h.Write([]byte(password))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Token3 computes the third-generation InSales autologin token.
// Formula:
//
//	MD5(nonce + user_email + user_name + user_id + email_confirmed + password)
//
// email_confirmed must be the literal string "true" or "false".
// user_name can be empty. All fields are UTF-8, concatenated without separators.
func Token3(nonce, userEmail, userName, userID, emailConfirmed, password string) string {
	h := md5.New() // #nosec G401 -- Required for InSales protocol compatibility
	h.Write([]byte(nonce))
	h.Write([]byte(userEmail))
	h.Write([]byte(userName))
	h.Write([]byte(userID))
	h.Write([]byte(emailConfirmed))
	h.Write([]byte(password))
	return fmt.Sprintf("%x", h.Sum(nil))
}
