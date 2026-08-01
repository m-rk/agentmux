package provision

// checkClaudeTokenExpiry has no macOS implementation: Claude Code stores
// credentials in Keychain there, not a plain file, and reading a specific
// Keychain item programmatically deserves the same care as any other secret
// access — deferred until that's verified rather than shipped blind.
// Callers should fall back to a plain logged-in/logged-out check (e.g.
// claudeLoggedIn) for macOS instances.
func checkClaudeTokenExpiry(_ string) (TokenExpiryStatus, error) {
	return TokenExpiryStatus{}, nil
}
