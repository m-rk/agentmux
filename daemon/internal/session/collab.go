package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/collab"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

const (
	collabSyncTimeout = 12 * time.Second
	collabIdleStable  = 3 * time.Second
	collabIdleTimeout = 6 * time.Second
)

// syncCollaboration polls relevant Discord forum threads and, when the pane
// is idle, delivers a compact untrusted-input digest. It is called only for a
// session that already existed when the periodic run began; fresh launches
// receive the onboarding prompt on their first tick instead of extending the
// service startup critical path.
func syncCollaboration(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	configPath := discordnotify.DefaultPath()
	if configPath == "" {
		return nil
	}
	cfg, err := discordnotify.Load(configPath)
	if err != nil {
		return err
	}
	if !cfg.Collaboration.Configured() {
		return nil
	}

	agent := agentOf(fields)
	if !collaborationSupported(agent) {
		return nil
	}
	host := fields["AGENTMUX_HOST_NAME"]
	if host == "" {
		host = provision.DefaultHostName()
	}
	avatar := cfg.Collaboration.SessionAvatarURLs[name]
	if avatar == "" {
		avatar = fields["AGENTMUX_DISCORD_AVATAR_URL"]
	}
	if avatar == "" {
		avatar = cfg.Collaboration.AgentAvatarURLs[agent]
	}
	if err := collab.ValidateAvatarURL(avatar); err != nil {
		return err
	}
	projectOverride := cfg.Collaboration.ProjectKeys[name]
	if projectOverride == "" {
		projectOverride = fields["AGENTMUX_PROJECT"]
	}
	project, err := collab.DetectProject(fields["AGENTMUX_WORKDIR"], projectOverride, withPath)
	if err != nil {
		return err
	}

	fallback := name
	if agent == "claude-code" {
		fallback = "agentmux"
	}
	session := sessionNameOf(fields, fallback)
	socket := tmuxSocket(name)
	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }
	sessionKeyBytes, err := tmux("-L", socket, "display-message", "-p", "-t", session, "#{session_created}").Output()
	if err != nil {
		return fmt.Errorf("reading tmux session identity: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving collaboration state home: %w", err)
	}
	identity := collab.Identity{Instance: name, Host: host, Agent: agent, AvatarURL: avatar}
	ctx, cancel := context.WithTimeout(context.Background(), collabSyncTimeout)
	defer cancel()
	delivery, err := collab.BuildDelivery(ctx, collab.NewClient(cfg.Collaboration), collab.SyncOptions{
		Identity:  identity,
		Project:   project,
		StatePath: collab.StatePath(home, name),
	})
	if err != nil {
		return err
	}
	if delivery.Prompt == "" {
		if delivery.StateChanged {
			return collab.SaveState(collab.StatePath(home, name), delivery.State)
		}
		return nil
	}
	delivered := false
	// Non-blocking: the nightly compact, or a Remote Control reconnect from
	// this same tick, may already be mid-send to this pane. Skip this tick
	// rather than risk our digest text landing in its still-unsubmitted
	// input line — the next tick, a few minutes away, retries with cursors
	// untouched.
	_, err = withTmuxInputLock(home, nil, name, false, func() error {
		if err := waitForPaneIdle(tmux, socket, session, collabIdleStable, collabIdleTimeout); err != nil {
			// A busy session is healthy; leave cursors untouched and retry later.
			return nil
		}
		pane, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
		if err != nil || !collaborationPaneSafe(agent, string(pane)) {
			// Stability alone is insufficient: a permission or selection dialog
			// can sit unchanged too. Never let an automatic Enter answer one.
			return nil
		}
		currentKey, err := tmux("-L", socket, "display-message", "-p", "-t", session, "#{session_created}").Output()
		if err != nil || strings.TrimSpace(string(currentKey)) != strings.TrimSpace(string(sessionKeyBytes)) {
			return nil
		}
		if safesend.Classify(agent, string(pane)) == safesend.StateDraft {
			// Someone's real draft (or a stuck earlier paste): typing on top of it
			// would merge the two. Skip; the next tick retries.
			return nil
		}
		if err := deliverCollabPrompt(tmux, socket, session, agent, delivery.Prompt); err != nil {
			return fmt.Errorf("delivering Discord collaboration to %s: %w", session, err)
		}
		delivered = true
		return nil
	})
	if err != nil {
		return err
	}
	if !delivered {
		return nil
	}
	return collab.SaveState(collab.StatePath(home, name), delivery.State)
}

// collaborationSupported reports whether an agent has an interactive pane a
// Discord digest could be delivered into at all. Everything except amp does:
// claude-code, zero, opencode and kilo all run a TUI whose input line
// send-keys can type a prompt into.
//
// amp is the exception because agentmux launches it with --no-tui — a
// headless runner that serves remotely created threads and never renders a
// prompt. Text sent to that pane would land on the runner process's stdin as
// noise, so there is no version of this that works, and no pane state that
// would make it work later. Checked here as well as in
// collaborationPaneSafe so an amp instance doesn't poll Discord on every
// five-minute tick for a delivery that can never happen.
func collaborationSupported(agent string) bool {
	return agent != "amp" && agent != "codex"
}

func collaborationPaneSafe(agent, pane string) bool {
	if strings.TrimSpace(pane) == "" {
		return false
	}
	switch agent {
	case "claude-code":
		if !ClaudePaneRemoteConnected(pane) {
			return false
		}
	case "kilo":
		if !KiloPaneReady(pane) {
			return false
		}
	case "amp", "codex":
		// Unconditional, unlike the readiness gates above: see
		// collaborationSupported. No amp pane is ever a safe delivery target,
		// however idle and dialog-free it looks.
		return false
	}
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	if len(lines) > 14 {
		lines = lines[len(lines)-14:]
	}
	footer := strings.ToLower(strings.Join(lines, "\n"))
	for _, marker := range []string{
		"do you want to",
		"enter to select",
		"esc to continue",
		"esc to cancel",
		"allow once",
		"allow for this",
		"permission required",
		"add credential",
		"are you sure",
		"[y/n]",
		"[yes/no]",
		"(y/n)",
	} {
		if strings.Contains(footer, marker) {
			return false
		}
	}
	return true
}

// collabSubmitSettle is how long the TUI gets to finish handling a paste
// before Enter, and between submit checks.
var collabSubmitSettle = 300 * time.Millisecond

// collabSubmitAttempts bounds the Enter retries before the paste is cleared.
const collabSubmitAttempts = 3

// deliverCollabPrompt pastes prompt as one bracketed paste (Claude Code folds
// a multi-line paste into "[Pasted text #1 +N lines]" and absorbs an Enter
// sent in the same tmux call), presses Enter after a settle delay, and then
// checks the input line is empty, retrying Enter a few times. It returns an
// error if the prompt is still sitting unsent, so callers don't record it as
// delivered.
func deliverCollabPrompt(tmux func(args ...string) *exec.Cmd, socket, session, agent, prompt string) error {
	buffer := fmt.Sprintf("agentmux-collab-%d", time.Now().UnixNano())
	load := tmux("-L", socket, "load-buffer", "-b", buffer, "-")
	load.Stdin = strings.NewReader(prompt)
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("loading digest: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := tmux("-L", socket, "paste-buffer", "-p", "-d", "-b", buffer, "-t", session).CombinedOutput(); err != nil {
		_ = tmux("-L", socket, "delete-buffer", "-b", buffer).Run()
		return fmt.Errorf("pasting digest: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for attempt := 0; attempt < collabSubmitAttempts; attempt++ {
		time.Sleep(collabSubmitSettle)
		if out, err := tmux("-L", socket, "send-keys", "-t", session, "Enter").CombinedOutput(); err != nil {
			return fmt.Errorf("submitting digest: %w: %s", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(collabSubmitSettle)
		pane, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
		if err != nil {
			return fmt.Errorf("checking digest was submitted: %w", err)
		}
		if safesend.Classify(agent, string(pane)) != safesend.StateDraft {
			return nil
		}
	}
	// Never leave our own paste behind: it would block the next send (or a
	// human) with what looks like a real draft. Pane was draft-free before the
	// paste (checked by the caller under the input lock), so whatever is in the
	// input line now is ours. One C-c clears Claude Code's input without
	// exiting (it only exits on an empty line); C-u covers other TUIs.
	cleared := clearCollabDraft(tmux, socket, session, agent)
	if cleared {
		return fmt.Errorf("digest was not submitted; cleared it from the input line, will retry next tick")
	}
	return fmt.Errorf("digest still unsent in the input line after submitting, and clearing it failed")
}

// clearCollabDraft empties the input line and reports whether the pane no
// longer shows a draft.
func clearCollabDraft(tmux func(args ...string) *exec.Cmd, socket, session, agent string) bool {
	for _, key := range []string{"C-c", "C-u"} {
		_ = tmux("-L", socket, "send-keys", "-t", session, key).Run()
		time.Sleep(collabSubmitSettle)
		pane, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
		if err == nil && safesend.Classify(agent, string(pane)) != safesend.StateDraft {
			return true
		}
	}
	return false
}
