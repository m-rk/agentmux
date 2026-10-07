package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/liveguard"
)

// codexIdleCommand is what a codex instance's tmux session runs: a
// placeholder that keeps the session (and so the instance's status, view and
// the doctor's liveness check) alive. Codex itself only runs headless, one
// `codex exec` per turn started by ops.Run, so nothing agent-specific lives
// in the pane. "codex-idle" is $0, there so the doctor's process-identity
// check (the process tree must mention the agent) finds "codex".
var codexIdleCommand = []string{"sh", "-c", "while :; do sleep 3600; done", "codex-idle"}

// RunCodex is `agentmux session run --instance NAME` for the codex agent:
// idempotently ensures the instance's tmux session exists. Like RunAmp it
// never types into the pane; a session already up is left untouched.
// Task instances get the codex wrapper first on PATH (see ensureTaskCodexStub)
// so a bare `codex exec` typed in the pane is refused.
func RunCodex(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	workdir := fields["AGENTMUX_WORKDIR"]
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("creating workdir %s: %w", workdir, err)
	}
	if hasSession(socket, session) {
		return nil
	}
	tmuxArgs := append([]string{"-L", socket, "new-session", "-d", "-s", session, "-c", workdir}, taskCodexStubArgs(name)...)
	tmuxArgs = append(tmuxArgs, codexIdleCommand...)
	if out, err := withPath("tmux", tmuxArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	return nil
}

// taskCodexStubArgs returns the tmux -e pair putting the task codex wrapper
// first on PATH for task-* instances; nothing for any other instance.
func taskCodexStubArgs(name string) []string {
	if !strings.HasPrefix(name, liveguard.TaskPrefix) {
		return nil
	}
	dir := taskAmpStubDir()
	if dir == "" {
		return nil
	}
	if err := ensureTaskCodexStub(dir); err != nil {
		fmt.Fprintf(os.Stderr, "%s: task codex wrapper: %v\n", name, err)
		return nil
	}
	return []string{"-e", "PATH=" + dir + ":$PATH"}
}

// StopCodex is the instance unit's ExecStop: kill the placeholder session
// and wait for it to go (as StopAmp). In-flight `codex exec` runs are
// detached on purpose and survive; retire stops them (StopCodexRuns).
func StopCodex(name string) error {
	return StopAmp(name)
}

// UpdateCodex is `agentmux session update` for codex. There is nothing to
// update or restart: the CLI is installed and upgraded by the operator, and
// every run starts a fresh `codex` process, so the next run picks up a new
// binary by itself. It only makes sure the session exists.
func UpdateCodex(name string) error {
	return RunCodex(name)
}
