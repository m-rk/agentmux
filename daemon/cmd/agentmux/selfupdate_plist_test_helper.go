package main

// renderSelfUpdatePlistForTest renders the plist with fixed paths, so
// tests assert the template without repeating install defaults. The
// template is platform-independent; only installing it is darwin-gated.
func renderSelfUpdatePlistForTest(cfg selfUpdateHostConfig) string {
	return renderSelfUpdatePlist("/home/me/.agentmux/bin/agentmux", cfg, "/home/me/.agentmux/log")
}
