package session

import "fmt"

// updateClaudeCode runs as the instance's own user already (macOS
// LaunchAgents are per-user; no privilege drop needed) and restarts by
// calling StopClaudeCode/RunClaudeCode directly — there's no service
// manager to delegate a "restart" to. Re-kickstarting the RunAtLoad
// LaunchAgent instead wouldn't work: RunClaudeCode is intentionally
// idempotent (a no-op if the tmux session is already up), which is exactly
// wrong right after an update — the whole point here is to kill the stale
// session so the next one picks up the newly installed claude binary.
func updateClaudeCode(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, "agentmux")
	socket := tmuxSocket(name)

	before, _ := withPath("claude", "--version").CombinedOutput()
	if err := withPath("claude", "update").Run(); err != nil {
		return fmt.Errorf("claude update failed, leaving existing session running untouched: %w", err)
	}
	after, _ := withPath("claude", "--version").CombinedOutput()

	if string(before) == string(after) && hasSession(socket, session) {
		return nil // no version change, session already running
	}
	if err := StopClaudeCode(name); err != nil {
		return fmt.Errorf("stopping %s before restart: %w", name, err)
	}
	return RunClaudeCode(name)
}
