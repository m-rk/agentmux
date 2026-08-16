package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// refuseIfSelfTarget blocks a local restart/stop/re-provision of the exact
// instance this process is currently running inside of. Confirmed live: a
// kilo agent ran `agentmux new -y -instance agentmux-kilo ...` on itself to
// "fix" its own session, which tore down its own tmux server mid-command —
// the shell command's output pipe was severed before the CLI could report
// back, leaving the session's tool-call state permanently stuck at
// "running" on every future resume. tmux sets $TMUX to
// "<socket-path>,<pid>,<session-index>" inside any pane it manages;
// agentmux's own sockets are named "agentmux-<instance>" (see tmuxSocket in
// daemon/internal/session/session.go), so a match there means this process
// is a descendant of the very session it's about to restart. Only meaningful
// for "local" — a remote host's instance can't be the session this process
// is running in.
func refuseIfSelfTarget(host, instance string, force bool) error {
	if force || instance == "" || (host != "" && host != "local") {
		return nil
	}
	tmuxVar := os.Getenv("TMUX")
	if tmuxVar == "" {
		return nil
	}
	socketPath := tmuxVar
	if i := strings.IndexByte(tmuxVar, ','); i >= 0 {
		socketPath = tmuxVar[:i]
	}
	if filepath.Base(socketPath) != "agentmux-"+instance {
		return nil
	}
	return fmt.Errorf("refusing to target instance %q: this process is running inside that instance's own tmux session, and restarting it would tear itself down mid-command; pass -force to override, or ask another instance/session to do it", instance)
}
