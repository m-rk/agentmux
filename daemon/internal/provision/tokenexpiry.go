package provision

import "time"

// TokenExpiryStatus reports what's known about an instance's OAuth token
// expiry. Supported is false when this platform/agent combination has no
// known way to read expiry timestamps (currently: any agent on macOS, where
// Claude Code's credentials live in Keychain rather than a plain file) —
// callers should fall back to a plain logged-in/logged-out check in that
// case, via claudeLoggedIn or similar. AccessExpiresAt/RefreshExpiresAt are
// the zero time.Time when Supported is true but that particular field
// wasn't found in the credential store.
type TokenExpiryStatus struct {
	Supported        bool
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// CheckTokenExpiry reports agent's OAuth token expiry for runUser, dispatching
// to a platform-specific reader — see tokenexpiry_linux.go/
// tokenexpiry_darwin.go. Only claude-code has OAuth expiry to track today;
// other agents report Supported: false.
func CheckTokenExpiry(agent, runUser string) (TokenExpiryStatus, error) {
	if agent != "claude-code" {
		return TokenExpiryStatus{}, nil
	}
	return checkClaudeTokenExpiry(runUser)
}
