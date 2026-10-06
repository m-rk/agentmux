//go:build !darwin

package main

// selfUpdatePlistSupported reports whether this build renders the
// launchd plist (darwin-only). Tests skip plist rendering elsewhere.
const selfUpdatePlistSupported = false

// renderSelfUpdatePlistForTest is unavailable off darwin.
func renderSelfUpdatePlistForTest(_ selfUpdateHostConfig) string { return "" }
