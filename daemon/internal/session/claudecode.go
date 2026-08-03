package session

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
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
	// remoteControlIdleStable/-Timeout gate ensureClaudeRemoteControl's
	// keystrokes behind a much shorter idle check than waitForPaneIdle's
	// overnight-friendly defaults above: this runs on every periodic tick
	// (every few minutes on macOS), so a busy pane just means "try again
	// next tick" rather than something worth blocking on.
	remoteControlIdleStable  = 5 * time.Second
	remoteControlIdleTimeout = 15 * time.Second
	// authExpiryWarnWindow is how far ahead of a refresh-token expiry
	// ensureClaudeAuthNotified fires its "expiring soon" warning — long
	// enough that the user has a real chance to re-authenticate (run
	// `claude` once) before the session actually breaks.
	authExpiryWarnWindow = 48 * time.Hour
)

// claudeRemoteIndicator is the footer text Claude Code shows only while
// Remote Control is actually connected — confirmed live, not guessed from
// bundle strings: a session started without --remote-control shows no such
// text at all, and the "/remote-control" palette entry itself reads
// "Disconnect Remote Control" while a session is connected.
const claudeRemoteIndicator = "/rc"

// claudeFooterScanLines bounds Remote Control status/menu detection to the
// bottom of Claude Code's TUI. The status area is not a single fixed row: mode,
// model, effort, and keyboard-hint rows can render below /rc or the menu prompt.
// Six rows covers the observed variants while staying well clear of the
// welcome box, where workdir text must not be mistaken for connection state.
const claudeFooterScanLines = 6

// compactOnUpdateEnabled reports whether the nightly update should compact
// and always restart (true, the default — preserves behavior for any
// instance that doesn't set this field, including everything provisioned
// before this setting existed) or fall back to the old
// version-change-only restart behavior (false). See
// CreateInstanceRequest.compact_on_update's doc comment in the proto for
// this field's values.
func compactOnUpdateEnabled(fields map[string]string) bool {
	switch fields["AGENTMUX_COMPACT_ON_UPDATE"] {
	case "off", "0", "false", "no":
		return false
	default:
		return true
	}
}

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

	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }

	if hasSession(socket, session) {
		ensureClaudeAuthNotified(name, fields["AGENTMUX_RUN_USER"], display)
		return ensureClaudeRemoteControl(tmux, socket, session)
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
	// A fresh --resume can itself land on Claude Code's own Remote Control
	// confirmation menu (see ensureClaudeRemoteControl's doc comment) with no
	// self-recovery of its own, and the periodic tick that would otherwise
	// catch it may be minutes away. Give it one immediate chance here too —
	// ensureClaudeRemoteControl already defers harmlessly (returns nil) if
	// the pane is still busy replaying a large transcript, in which case the
	// next tick covers it exactly as before.
	return ensureClaudeRemoteControl(tmux, socket, session)
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

// claudeRemoteConnected checks a bounded footer window for /rc as an exact
// whitespace-delimited token. Checking the whole pane is unsafe: workdir text
// renders in the welcome box and can contain lookalike "rc" strings. Claude's
// status area can span several rows, though, so checking only the literal last
// row misses a real /rc whenever a mode/model hint renders beneath it.
func claudeRemoteConnected(tmux func(args ...string) *exec.Cmd, socket, session string) bool {
	for _, field := range strings.Fields(lastPaneLines(tmux, socket, session, claudeFooterScanLines)) {
		if field == claudeRemoteIndicator {
			return true
		}
	}
	return false
}

// claudeRemoteMenuFooter is the prompt on Claude Code's Remote Control
// confirmation menu (Disconnect this session / Show QR code / Continue),
// which /remote-control opens whenever it's invoked while already connected
// (confirmed live against v2.1.220 — this is not the same as the plain
// connect/disconnect toggle applied when currently disconnected, which takes
// effect immediately with no menu). While this menu is up it covers the
// footer window claudeRemoteConnected reads, so a disconnected-looking pane
// may really just be this menu sitting on top of a still-live connection.
const claudeRemoteMenuFooter = "Esc to continue"

func claudeRemoteMenuOpen(tmux func(args ...string) *exec.Cmd, socket, session string) bool {
	return strings.Contains(lastPaneLines(tmux, socket, session, claudeFooterScanLines), claudeRemoteMenuFooter)
}

// dismissClaudeRemoteMenuIfOpen closes the Remote Control menu via Escape —
// the same as selecting its default-highlighted "Continue" entry, which
// leaves the connection exactly as it already was. That makes this always
// safe to call speculatively: it's a no-op if the menu isn't open, and a
// no-op on connection state if it is.
func dismissClaudeRemoteMenuIfOpen(tmux func(args ...string) *exec.Cmd, socket, session string) (bool, error) {
	if !claudeRemoteMenuOpen(tmux, socket, session) {
		return false, nil
	}
	if err := tmux("-L", socket, "send-keys", "-t", session, "Escape").Run(); err != nil {
		return false, fmt.Errorf("dismissing remote control menu in %s: %w", session, err)
	}
	return true, nil
}

func lastPaneLines(tmux func(args ...string) *exec.Cmd, socket, session string, count int) string {
	if count <= 0 {
		return ""
	}
	out, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	start := len(lines) - count
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:], "\n")
}

// ensureClaudeRemoteControl works around Claude Code's own Remote Control
// bug (anthropics/claude-code#31853): the server drops the websocket every
// ~25 minutes and, after the third drop, gives up reconnecting for good with
// no client-side recovery. The fix is a single idempotent slash command, so
// it's cheap enough to run on every periodic `session run` tick rather than
// waiting for the nightly update.
//
// It also guards against Claude Code's confirmation menu (see
// claudeRemoteMenuFooter): if a previous toggle — ours or a human's/another
// agent's — left that menu open, claudeRemoteConnected can't see the "/rc"
// footer behind it and would otherwise report "disconnected" forever and
// leave the pane visibly wedged on the menu. Dismissing it before and after
// toggling makes this self-healing regardless of how the menu got there.
func ensureClaudeRemoteControl(tmux func(args ...string) *exec.Cmd, socket, session string) error {
	if claudeRemoteConnected(tmux, socket, session) {
		return nil
	}
	if dismissed, err := dismissClaudeRemoteMenuIfOpen(tmux, socket, session); err != nil {
		return err
	} else if dismissed {
		time.Sleep(500 * time.Millisecond)
		if claudeRemoteConnected(tmux, socket, session) {
			return nil // the menu was hiding an already-live connection
		}
	}
	// Never type into a pane mid-response; if it's busy, skip this tick and
	// let the next one (a few minutes away) retry instead of blocking.
	if err := waitForPaneIdle(tmux, socket, session, remoteControlIdleStable, remoteControlIdleTimeout); err != nil {
		return nil
	}
	if claudeRemoteConnected(tmux, socket, session) {
		return nil // reconnected on its own while we were checking idle
	}
	if err := tmux("-L", socket, "send-keys", "-t", session, "/remote-control", "Enter").Run(); err != nil {
		return fmt.Errorf("sending /remote-control to %s: %w", session, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if claudeRemoteConnected(tmux, socket, session) {
			return nil
		}
		// Disconnected really means disconnected here (unlike above), so
		// invoking /remote-control should reconnect immediately with no
		// menu. Seeing the menu instead means the state flipped to
		// connected in the race between our checks and the send-keys above;
		// dismissing leaves it connected, which is what we want.
		if dismissed, err := dismissClaudeRemoteMenuIfOpen(tmux, socket, session); err != nil {
			return err
		} else if dismissed {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("remote control still not connected in %s after toggling", session)
}

// ensureClaudeAuthNotified checks name's Claude Code OAuth token expiry
// (Linux only today — see provision.CheckTokenExpiry's doc comment on the
// macOS gap) and sends a Discord notification the first time it crosses
// into "expiring soon" or "already expired," debounced via two registry
// fields so a 5-minute tick doesn't resend the same notification forever.
// Never returns an error: a broken Discord webhook or an unsupported
// platform shouldn't stop RunClaudeCode's own tmux/remote-control self-heal,
// so problems here are logged, not propagated.
func ensureClaudeAuthNotified(name, runUser, displayName string) {
	status, err := provision.CheckTokenExpiry("claude-code", runUser)
	if err != nil {
		fmt.Printf("warning: checking token expiry for %s: %v\n", name, err)
		return
	}
	if !status.Supported {
		return
	}

	fields, err := registry(name)
	if err != nil {
		fmt.Printf("warning: re-reading registry for %s: %v\n", name, err)
		return
	}

	now := time.Now()
	expired := !status.RefreshExpiresAt.IsZero() && now.After(status.RefreshExpiresAt)
	expiringSoon := !expired && !status.RefreshExpiresAt.IsZero() && status.RefreshExpiresAt.Sub(now) < authExpiryWarnWindow
	notifiedExpired := fields["AGENTMUX_AUTH_NOTIFIED_EXPIRED"] == "true"
	notifiedSoon := fields["AGENTMUX_AUTH_NOTIFIED_EXPIRING"] == "true"

	notify := func(msg string) {
		cfg, err := discordnotify.Load(discordnotify.DefaultPath())
		if err != nil || cfg.WebhookURL == "" {
			return
		}
		if err := discordnotify.Send(cfg.WebhookURL, msg); err != nil {
			fmt.Printf("warning: sending Discord notification for %s: %v\n", name, err)
		}
	}

	switch {
	case expired && !notifiedExpired:
		notify(fmt.Sprintf("🔴 %s: Claude Code's refresh token has expired — the session will stop working on its next token refresh. Run `claude` and log in again.", displayName))
		_ = SetRegistryField(name, "AGENTMUX_AUTH_NOTIFIED_EXPIRED", "true")
	case expiringSoon && !notifiedSoon:
		notify(fmt.Sprintf("🟡 %s: Claude Code's refresh token expires %s — re-authenticate soon (run `claude` and log in) to avoid an interruption.", displayName, status.RefreshExpiresAt.Format(time.RFC1123)))
		_ = SetRegistryField(name, "AGENTMUX_AUTH_NOTIFIED_EXPIRING", "true")
	case !expired && !expiringSoon:
		// Healthy again (e.g. the user re-authenticated) — clear both
		// flags so a future expiry gets a fresh notification instead of
		// staying silenced forever.
		if notifiedExpired {
			_ = SetRegistryField(name, "AGENTMUX_AUTH_NOTIFIED_EXPIRED", "false")
		}
		if notifiedSoon {
			_ = SetRegistryField(name, "AGENTMUX_AUTH_NOTIFIED_EXPIRING", "false")
		}
	}
}

func hasSession(socket, session string) bool {
	return withPath("tmux", "-L", socket, "has-session", "-t", session).Run() == nil
}

func hasSessionAs(runUser, socket, session string) bool {
	return runAs(runUser, "tmux", "-L", socket, "has-session", "-t", session).Run() == nil
}

// compactAndResolveResume compacts the live session (if one is running and
// isn't already sitting on a compact boundary from a previous update — see
// below) via tmux send-keys, waiting for it to go idle first (so /compact
// doesn't land mid-response) and again afterward (so we don't restart
// before compaction finishes), then figures out which session ID a
// subsequent restart should --resume: the most recently modified session
// file for the instance's workdir (the same ~/.claude/projects scan
// ListResumableSessions uses), preferred over the registry's own
// AGENTMUX_RESUME field, which is only ever set once — at creation time,
// and only if the wizard's resume picker was used, so it's empty for most
// instances. Whatever's found gets persisted back into the registry, so
// the next restart doesn't need to look it up again.
//
// This is what keeps a long-lived session small enough that a later
// --resume never hits Claude Code's own "this session is huge, are you
// sure?" interactive prompt — which would otherwise leave the instance
// stuck waiting for input no one's there to give.
//
// If nothing happened in the session since the last time it was compacted
// (the common case for an instance that sat idle overnight-to-overnight),
// its transcript's last message is already the compact-boundary summary
// from that earlier run, and sending another /compact would be a pure
// no-op — Claude Code refuses it outright ("Not enough messages to
// compact."), which would otherwise burn idleWaitTimeout/compactTimeout
// waiting on a prompt that was never going anywhere. So that case skips
// straight to resolving the resume ID.
//
// tmux is the caller's own tmux-command builder, already carrying the
// right privilege-drop/PATH setup for its context (root-context Linux
// update vs. current-user-context macOS update).
func compactAndResolveResume(tmux func(args ...string) *exec.Cmd, name, workdir, runUser, socket, session string) (string, error) {
	if tmux("-L", socket, "has-session", "-t", session).Run() == nil {
		alreadyCompacted, err := provision.LastMessageIsCompactSummary(workdir, runUser)
		if err != nil {
			return "", fmt.Errorf("checking whether %s is already compacted: %w", session, err)
		}
		if !alreadyCompacted {
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
		if err := SetRegistryField(name, "AGENTMUX_RESUME", resumeID); err != nil {
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
