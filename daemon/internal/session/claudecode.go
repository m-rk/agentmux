package session

import (
	"fmt"
	"os"
	"os/exec"
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

// UpdateClaudeCode is `agentmux session update --instance NAME`: checks
// for a new Claude Code version and restarts the session only if it
// changed or the session isn't running, matching rc-update.sh. Runs as
// root (it needs to call systemctl), dropping to the instance's run user
// only for the claude/tmux calls themselves.
func UpdateClaudeCode(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	runUser := fields["AGENTMUX_RUN_USER"]
	serviceName := fields["AGENTMUX_SERVICE_NAME"]
	if runUser == "" || serviceName == "" {
		return fmt.Errorf("registry for %s is missing AGENTMUX_RUN_USER/AGENTMUX_SERVICE_NAME", name)
	}
	session := sessionNameOf(fields, "agentmux")
	socket := tmuxSocket(name)

	before, _ := runAs(runUser, "claude", "--version").CombinedOutput()
	if err := runAs(runUser, "claude", "update").Run(); err != nil {
		return fmt.Errorf("claude update failed, leaving existing session running untouched: %w", err)
	}
	after, _ := runAs(runUser, "claude", "--version").CombinedOutput()

	if string(before) == string(after) && hasSessionAs(runUser, socket, session) {
		return nil // no version change, session already running
	}
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		return fmt.Errorf("restarting %s: %w", serviceName, err)
	}
	return nil
}

func hasSession(socket, session string) bool {
	return exec.Command("tmux", "-L", socket, "has-session", "-t", session).Run() == nil
}

func hasSessionAs(runUser, socket, session string) bool {
	return runAs(runUser, "tmux", "-L", socket, "has-session", "-t", session).Run() == nil
}
