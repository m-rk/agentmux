package session

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/provision"
)

const (
	// idleStableWindow mirrors discovery's own idle threshold: how long a
	// pane's content must sit unchanged before we consider the agent done
	// responding and safe to type into.
	idleStableWindow = 30 * time.Second
	// idleWaitTimeout is generous because this runs unattended overnight —
	// blocking a few minutes for a long tool-use loop to finish is fine.
	idleWaitTimeout = 5 * time.Minute
	// compactTimeout accommodates a large (hundreds-of-thousands-of-tokens)
	// session taking a while to compact.
	compactTimeout = 10 * time.Minute
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

// compactAndResolveResume compacts the live session (if one is running) via
// tmux send-keys, waiting for it to go idle first (so /compact doesn't land
// mid-response) and again afterward (so we don't restart before compaction
// finishes), then figures out which session ID a subsequent restart should
// --resume: the most recently modified session file for the instance's
// workdir (the same ~/.claude/projects scan ListResumableSessions uses),
// preferred over the registry's own AGENTMUX_RESUME field, which is only
// ever set once — at creation time, and only if the wizard's resume picker
// was used, so it's empty for most instances. Whatever's found gets
// persisted back into the registry, so the next restart doesn't need to
// look it up again.
//
// This is what keeps a long-lived session small enough that a later
// --resume never hits Claude Code's own "this session is huge, are you
// sure?" interactive prompt — which would otherwise leave the instance
// stuck waiting for input no one's there to give.
//
// tmux is the caller's own tmux-command builder, already carrying the
// right privilege-drop/PATH setup for its context (root-context Linux
// update vs. current-user-context macOS update).
func compactAndResolveResume(tmux func(args ...string) *exec.Cmd, name, workdir, runUser, socket, session string) (string, error) {
	if tmux("-L", socket, "has-session", "-t", session).Run() == nil {
		if err := waitForPaneIdle(tmux, socket, session, idleStableWindow, idleWaitTimeout); err != nil {
			return "", fmt.Errorf("waiting for %s to go idle before compacting: %w", session, err)
		}
		if err := tmux("-L", socket, "send-keys", "-t", session, "/compact", "Enter").Run(); err != nil {
			return "", fmt.Errorf("sending /compact to %s: %w", session, err)
		}
		time.Sleep(3 * time.Second) // let compaction visibly start before polling for idle again
		if err := waitForPaneIdle(tmux, socket, session, idleStableWindow, compactTimeout); err != nil {
			return "", fmt.Errorf("waiting for %s to finish compacting: %w", session, err)
		}
	}

	sessions, err := provision.ListResumable(workdir, runUser)
	if err != nil {
		return "", fmt.Errorf("listing resumable sessions for %s: %w", workdir, err)
	}
	resumeID := ""
	if len(sessions) > 0 {
		resumeID = sessions[0].SessionID // newest first
	} else {
		fields, _ := registry(name)
		resumeID = fields["AGENTMUX_RESUME"]
	}
	if resumeID != "" {
		if err := setRegistryField(name, "AGENTMUX_RESUME", resumeID); err != nil {
			return "", fmt.Errorf("persisting resume id for %s: %w", name, err)
		}
	}
	return resumeID, nil
}

// waitForPaneIdle polls session's pane content until it hasn't changed for
// stableFor, or returns an error once timeout elapses — a wedged/looping
// agent shouldn't block a nightly maintenance run forever.
func waitForPaneIdle(tmux func(args ...string) *exec.Cmd, socket, session string, stableFor, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastHash uint64
	lastChanged := time.Now()
	first := true
	for {
		out, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
		if err != nil {
			return fmt.Errorf("capturing pane: %w", err)
		}
		h := fnv.New64a()
		h.Write(out)
		sum := h.Sum64()

		now := time.Now()
		if first || sum != lastHash {
			lastHash = sum
			lastChanged = now
			first = false
		}
		if now.Sub(lastChanged) >= stableFor {
			return nil
		}
		if now.After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(2 * time.Second)
	}
}
