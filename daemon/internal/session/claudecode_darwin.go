package session

import (
	"fmt"
	"os/exec"
)

// updateClaudeCode runs as the instance's own user already (macOS
// LaunchAgents are per-user; no privilege drop needed) and restarts by
// calling StopClaudeCode/RunClaudeCode directly — there's no service
// manager to delegate a "restart" to. Re-kickstarting the RunAtLoad
// LaunchAgent instead wouldn't work: RunClaudeCode is intentionally
// idempotent (a no-op if the tmux session is already up), which is exactly
// wrong right after an update — the whole point here is to kill the stale
// session so the next one picks up the newly installed claude binary.
//
// By default (AGENTMUX_COMPACT_ON_UPDATE unset or "on") every run compacts
// and restarts the session regardless of whether the Claude Code version
// changed (see compactAndResolveResume's doc comment for why); set to
// "off" to fall back to the old version-change-only restart behavior.
func updateClaudeCode(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	session := sessionNameOf(fields, "agentmux")
	socket := tmuxSocket(name)

	before, _ := withPath("claude", "--version").CombinedOutput()
	if err := withPath("claude", "update").Run(); err != nil {
		return fmt.Errorf("claude update failed, leaving existing session running untouched: %w", err)
	}
	after, _ := withPath("claude", "--version").CombinedOutput()
	changed := string(before) != string(after)
	if changed {
		fmt.Printf("%s: claude updated %s -> %s\n", name, before, after)
	}

	if !compactOnUpdateEnabled(fields) {
		if !changed && hasSession(socket, session) {
			return nil // no version change, session already running
		}
		if err := StopClaudeCode(name); err != nil {
			return fmt.Errorf("stopping %s before restart: %w", name, err)
		}
		return RunClaudeCode(name)
	}

	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }
	if _, err := compactAndResolveResume(tmux, name, workdir, "", socket, session); err != nil {
		return fmt.Errorf("compacting %s: %w", name, err)
	}
	if err := StopClaudeCode(name); err != nil {
		return fmt.Errorf("stopping %s before restart: %w", name, err)
	}
	return RunClaudeCode(name)
}
