package session

import (
	"fmt"
	"os"
)

// RunClaudeCode is `agentmux session run --instance NAME` for the
// claude-code agent: idempotently ensures the instance's tmux session is
// running the claude CLI with Remote Control (and --resume, if the
// registry has one), matching backends/claude-code/rc-start.sh. Runs as
// the instance's target user already (the unit's User= directive), so no
// privilege dropping is needed here.
func RunClaudeCode(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, "agentmux")
	socket := tmuxSocket(name)
	workdir := fields["AGENTMUX_WORKDIR"]
	display := fields["AGENTMUX_DISPLAY_NAME"]
	if display == "" {
		display = session
	}
	resume := fields["AGENTMUX_RESUME"]

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	if hasSession(socket, session) {
		return nil
	}

	claudeArgs := []string{"--remote-control", display}
	if resume != "" {
		claudeArgs = append(claudeArgs, "--resume", resume)
	}
	// exec.Command takes args as a slice, not a shell string, so unlike
	// rc-start.sh there's no manual shell-quoting to get right here.
	tmuxArgs := append([]string{"-L", socket, "new-session", "-d", "-s", session, "-c", workdir, "claude"}, claudeArgs...)
	cmd := withPath("tmux", tmuxArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	return nil
}

// StopClaudeCode is the instance unit's ExecStop.
func StopClaudeCode(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, "agentmux")
	socket := tmuxSocket(name)
	_ = withPath("tmux", "-L", socket, "kill-session", "-t", session).Run()
	return nil
}

// UpdateClaudeCode is `agentmux session update --instance NAME`: checks for
// a new Claude Code version and restarts the session only if it changed or
// the session isn't running. Platform-specific (claudecode_linux.go /
// claudecode_darwin.go): Linux runs as root and needs runas to drop to the
// instance's run user plus systemctl to restart; macOS runs as the
// instance's own user already and restarts by calling StopClaudeCode/
// RunClaudeCode directly, with no service manager involved.
func UpdateClaudeCode(name string) error {
	return updateClaudeCode(name)
}

func hasSession(socket, session string) bool {
	return withPath("tmux", "-L", socket, "has-session", "-t", session).Run() == nil
}

func hasSessionAs(runUser, socket, session string) bool {
	return runAs(runUser, "tmux", "-L", socket, "has-session", "-t", session).Run() == nil
}
