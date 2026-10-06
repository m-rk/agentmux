//go:build darwin

package main

// selfUpdatePlistSupported reports whether this build renders the
// launchd plist (darwin-only). Tests skip plist rendering elsewhere.
const selfUpdatePlistSupported = true

// renderSelfUpdatePlistForTest renders the plist with fixed paths, so
// tests assert the template without repeating install defaults.
func renderSelfUpdatePlistForTest(cfg selfUpdateHostConfig) string {
	return renderSelfUpdatePlist("/home/me/.agentmux/bin/agentmux", cfg, "/home/me/.agentmux/log")
}
