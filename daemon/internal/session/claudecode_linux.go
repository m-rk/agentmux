package session

import (
	"fmt"
	"os/exec"
)

// updateClaudeCode runs as root (it needs to call systemctl), dropping to
// the instance's run user via runas for the claude/tmux calls themselves,
// matching rc-update.sh's own update check. By default (AGENTMUX_COMPACT_
// ON_UPDATE unset or "on") every run compacts and restarts the session
// regardless of whether the Claude Code version changed (see
// compactAndResolveResume's doc comment for why); set to "off" to fall
// back to the old version-change-only restart behavior instead.
func updateClaudeCode(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	runUser := fields["AGENTMUX_RUN_USER"]
	serviceName := fields["AGENTMUX_SERVICE_NAME"]
	workdir := fields["AGENTMUX_WORKDIR"]
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
	changed := string(before) != string(after)
	if changed {
		fmt.Printf("%s: claude updated %s -> %s\n", name, before, after)
	}

	if !compactOnUpdateEnabled(fields) {
		if changed || !hasSessionAs(runUser, socket, session) {
			if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
				return fmt.Errorf("restarting %s: %w", serviceName, err)
			}
		}
		return nil
	}

	tmux := func(args ...string) *exec.Cmd { return runAs(runUser, "tmux", args...) }
	if _, err := compactAndResolveResume(tmux, name, workdir, runUser, socket, session); err != nil {
		return fmt.Errorf("compacting %s: %w", name, err)
	}
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		return fmt.Errorf("restarting %s: %w", serviceName, err)
	}
	return nil
}
