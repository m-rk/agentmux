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
		out, err := tmux(
			"-L", socket,
			"send-keys", "-t", session, "-l", delivery.Prompt,
			";", "send-keys", "-t", session, "Enter",
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("delivering Discord collaboration to %s: %w: %s", session, err, strings.TrimSpace(string(out)))
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
