// Package session implements the per-instance runtime lifecycle (spawn,
// update-check, stop) that used to live in rc-start.sh/rc-update.sh,
// invoked by the installed unit's ExecStart/ExecStop as
// `agentmux session run|update|stop --instance NAME`.
package session

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
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

// withPath builds an *exec.Cmd for name/args, with PATH (and HOME) fixed
// up to include common per-user tool install locations, matching the PATH
// rc-start.sh/rc-update.sh export themselves — systemd/launchd don't
// inherit a login shell's PATH, and a bare Type=oneshot unit with only
// User= (no PAMName=) doesn't reliably set HOME either, so this resolves
// HOME via os/user rather than trusting $HOME.
//
// Unlike a naive "set cmd.Env after exec.Command(name, ...)", name is
// resolved against the fixed-up PATH explicitly before building the
// command: exec.Command's own lookup uses the *calling* process's ambient
// $PATH (os.Getenv, not cmd.Env), so a plain exec.Command("zero", ...) run
// from a unit with a minimal PATH would silently fail to find a
// user-installed binary even with cmd.Env correctly set — this is what
// actually broke both the claude tmux session (HOME missing) and a direct
// `zero providers check` call (PATH lookup happening too early) during
// testing; both are the same underlying Go gotcha.
func withPath(name string, args ...string) *exec.Cmd {
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	path := home + "/.local/bin:" + home + "/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:" + os.Getenv("PATH")

	resolved := name
	if !filepath.IsAbs(name) {
		if found := runas.SearchPath(name, path); found != "" {
			resolved = found
		}
	}

	cmd := exec.Command(resolved, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+path)
	return cmd
}

// runAs is runas.Command, used by session update (which runs from a
// root-context unit, since it needs root to call systemctl). session
// run/stop run from a unit whose User= directive already matches the
// target user, so they use withPath alone.
var runAs = runas.Command
