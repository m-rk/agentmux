// Package session implements the per-instance runtime lifecycle (spawn,
// update-check, stop) that used to live in rc-start.sh/rc-update.sh,
// invoked by the installed unit's ExecStart/ExecStop as
// `agentmux session run|update|stop --instance NAME`.
package session

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// registry reads name's registry file into a KEY=VALUE map, same format
// discovery.go parses.
func registry(name string) (map[string]string, error) {
	path := filepath.Join(discovery.EnvDir, name+".env")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading registry %s: %w", path, err)
	}
	defer f.Close()
	fields := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return fields, scanner.Err()
}

func tmuxSocket(name string) string { return "agentmux-" + name }

// agentOf mirrors discovery's own fallback: backends/claude-code doesn't
// set AGENTMUX_AGENT, so an empty value means claude-code.
func agentOf(fields map[string]string) string {
	if a := fields["AGENTMUX_AGENT"]; a != "" {
		return a
	}
	return "claude-code"
}

func agentFor(name string) (string, error) {
	fields, err := registry(name)
	if err != nil {
		return "", err
	}
	return agentOf(fields), nil
}

// Run, Update, and Stop dispatch to the right agent-specific
// implementation by peeking at the instance's own registry file — this is
// what `agentmux session run|update|stop --instance NAME` actually calls.
func Run(name string) error {
	agent, err := agentFor(name)
	if err != nil {
		return err
	}
	switch agent {
	case "claude-code":
		return RunClaudeCode(name)
	case "zero", "opencode":
		return RunAgentmux(name)
	default:
		return fmt.Errorf("unsupported agent %q for instance %q", agent, name)
	}
}

func Update(name string) error {
	agent, err := agentFor(name)
	if err != nil {
		return err
	}
	switch agent {
	case "claude-code":
		return UpdateClaudeCode(name)
	case "zero", "opencode":
		return UpdateAgentmux(name)
	default:
		return fmt.Errorf("unsupported agent %q for instance %q", agent, name)
	}
}

func Stop(name string) error {
	agent, err := agentFor(name)
	if err != nil {
		return err
	}
	switch agent {
	case "claude-code":
		return StopClaudeCode(name)
	case "zero", "opencode":
		return StopAgentmux(name)
	default:
		return fmt.Errorf("unsupported agent %q for instance %q", agent, name)
	}
}

func sessionNameOf(fields map[string]string, fallback string) string {
	if s := fields["AGENTMUX_TMUX_SESSION_NAME"]; s != "" {
		return s
	}
	if s := fields["AGENTMUX_SESSION_NAME"]; s != "" {
		return s
	}
	return fallback
}

// withPath is runas.CurrentUserCommand, kept as a local alias since every
// call site in this file predates that shared helper. See its doc comment
// for why a plain exec.Command(name, ...) isn't enough under systemd/
// launchd — this is what actually broke both the claude tmux session (HOME
// missing) and a direct `zero providers check` call (PATH lookup happening
// too early) during testing, and later broke discovery/Attach's own
// tmux calls the same way before they were pointed at this helper too.
var withPath = runas.CurrentUserCommand

// runAs is runas.Command, used by session update (which runs from a
// root-context unit, since it needs root to call systemctl). session
// run/stop run from a unit whose User= directive already matches the
// target user, so they use withPath alone.
var runAs = runas.Command
