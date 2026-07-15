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

func sessionNameOf(fields map[string]string, fallback string) string {
	if s := fields["AGENTMUX_TMUX_SESSION_NAME"]; s != "" {
		return s
	}
	if s := fields["AGENTMUX_SESSION_NAME"]; s != "" {
		return s
	}
	return fallback
}

// withPath fixes up cmd's PATH (and HOME) to include common per-user tool
// install locations, matching the PATH rc-start.sh/rc-update.sh export
// themselves — systemd/launchd don't inherit a login shell's PATH, and a
// bare Type=oneshot unit with only User= (no PAMName=) doesn't reliably
// set HOME either, so this resolves HOME via os/user rather than trusting
// $HOME: an empty HOME here silently breaks the PATH below (no
// .npm-global/bin), which breaks tmux's ability to find claude, which
// kills the pane and thus the whole session instantly — the exact bug
// this function exists to avoid.
func withPath(cmd *exec.Cmd) *exec.Cmd {
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+home+"/.local/bin:"+home+"/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:"+os.Getenv("PATH"),
	)
	return cmd
}

// runAs is runas.Command, used by session update (which runs from a
// root-context unit, since it needs root to call systemctl). session
// run/stop run from a unit whose User= directive already matches the
// target user, so they use withPath alone.
var runAs = runas.Command
