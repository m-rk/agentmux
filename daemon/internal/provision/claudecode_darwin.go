package provision

import "fmt"

// createClaudeCode on macOS is not implemented yet — Phase B only covers
// claude-code on Linux; the macOS port (LaunchAgent install, no-root
// preflight, ~/.agentmux/registry) is a follow-up phase. Stubbed here so
// the provision package still cross-compiles for macOS.
func createClaudeCode(opts Options) (string, error) {
	return "", fmt.Errorf("creating claude-code instances on macOS isn't implemented yet")
}
