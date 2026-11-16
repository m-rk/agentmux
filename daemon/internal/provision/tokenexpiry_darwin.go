package provision

import "time"

// checkClaudeTokenExpiry has no expiry timestamps on macOS: Claude Code
// stores credentials in Keychain there, not a plain file, and reading a
// specific Keychain item programmatically deserves the same care as any
// other secret access. It falls back to the plain logged-in/logged-out
// check (`claude auth status`), so a lost login is still reported as
// "expired" even though "expiring soon" can't be.
func checkClaudeTokenExpiry(_ string) (TokenExpiryStatus, error) {
	return claudeLoginExpiryStatus(claudeLoggedIn(), time.Now()), nil
}

// claudeLoginExpiryStatus maps a login check result to a TokenExpiryStatus.
// Logged in reports Supported with no timestamps (healthy: nothing expiring
// that we can see). Logged out reports a refresh expiry of now, which the
// caller treats as already expired.
func claudeLoginExpiryStatus(loginErr error, now time.Time) TokenExpiryStatus {
	if loginErr == nil {
		return TokenExpiryStatus{Supported: true}
	}
	return TokenExpiryStatus{Supported: true, RefreshExpiresAt: now}
}
