// Package session implements the per-instance runtime lifecycle (spawn,
// update-check, stop) that used to live in rc-start.sh/rc-update.sh,
// invoked by the installed unit's ExecStart/ExecStop as
// `agentmux session run|update|stop --instance NAME`.
package session

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// ReadRegistry exposes an instance's configuration to other agentmux CLI
// surfaces without duplicating the registry parser. The returned map is a
// copy populated by this read and can be changed by the caller.
func ReadRegistry(name string) (map[string]string, error) {
	return registry(name)
}

// SetRegistryField updates a single KEY=VALUE line in name's registry file
// in place (appending it if absent), leaving every other line untouched —
// symmetric with registry() above. Used to self-correct AGENTMUX_RESUME
// after discovering an instance's actual current session ID, since that
// field is otherwise only ever set once, at creation time (and usually
// isn't set at all, unless the wizard's resume picker was used).
func SetRegistryField(name, key, value string) error {
	if key == "" || strings.ContainsAny(key, "=\r\n\x00") {
		return fmt.Errorf("invalid registry key %q", key)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("registry value for %s contains a line break or NUL byte", key)
	}
	path := filepath.Join(discovery.EnvDir, name+".env")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading registry %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == key {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("writing registry %s: %w", path, err)
	}
	return nil
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
	started := time.Now()
	fields, err := registry(name)
	if err != nil {
		return err
	}
	agent := agentOf(fields)
	fallback := name
	if agent == "claude-code" {
		fallback = "agentmux"
	}
	// New sessions can take most of their service's startup deadline to
	// paint and connect. Their collaboration onboarding is safely deferred
	// to the next five-minute tick; existing sessions can receive updates
	// during this run.
	wasRunning := hasSession(tmuxSocket(name), sessionNameOf(fields, fallback))
	var runErr error
	switch agent {
	case "claude-code":
		runErr = RunClaudeCode(name)
	case "zero", "opencode", "kilo":
		runErr = RunAgentmux(name)
	case "amp":
		runErr = RunAmp(name)
	default:
		return fmt.Errorf("unsupported agent %q for instance %q", agent, name)
	}
	if runErr != nil {
		return runErr
	}
	// Claude's existing service units have a 30-second deadline. If the
	// primary health work consumed a meaningful part of that budget, leave
	// collaboration for the next tick rather than turning an additive
	// feature into a service failure.
	if wasRunning && time.Since(started) < 8*time.Second {
		if err := syncCollaboration(name); err != nil {
			// Discord collaboration is additive. A Discord outage or malformed
			// project config must never mark an otherwise healthy session failed.
			log.Printf("agentmux collaboration warning for %s: %v", name, err)
		}
	}
	return nil
}

func Update(name string) error {
	agent, err := agentFor(name)
	if err != nil {
		return err
	}
	switch agent {
	case "claude-code":
		return UpdateClaudeCode(name)
	case "zero", "opencode", "kilo":
		return UpdateAgentmux(name)
	case "amp":
		return UpdateAmp(name)
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
	case "zero", "opencode", "kilo":
		return StopAgentmux(name)
	case "amp":
		return StopAmp(name)
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
