package session

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// kiloReadyMarker is text kilo's TUI only renders once it's actually
	// interactive (the empty-input placeholder) — not present during the
	// cold-boot window (tmux pane exists but node/kilo hasn't drawn its
	// first frame yet), which idle-stability detection would wrongly
	// treat as "settled": a pane that hasn't started rendering is just as
	// unchanging as one that's finished. Confirmed against a live cold
	// start: the pane can sit blank/on the splash for several seconds
	// (model-list fetch, indexing) before this text ever appears.
	kiloReadyMarker       = "Ask anything"
	kiloReadyPollInterval = 500 * time.Millisecond
	kiloReadyTimeout      = 30 * time.Second
)

// RunAgentmux is `agentmux session run --instance NAME` for the zero/
// opencode agents: writes the provider config file, waits for the
// provider to be reachable, then idempotently ensures the instance's tmux
// session is running the agent CLI, matching
// backends/agentmux/rc-start.sh. Runs as the instance's target user
// already (the unit's User= directive).
func RunAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	agent := fields["AGENTMUX_AGENT"]
	provider := fields["AGENTMUX_PROVIDER"]
	model := fields["AGENTMUX_MODEL"]
	baseURL := fields["AGENTMUX_PROVIDER_BASE_URL"]
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	workdir := fields["AGENTMUX_WORKDIR"]
	waitSeconds := 60
	if s := fields["AGENTMUX_PROVIDER_WAIT_SECONDS"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			waitSeconds = n
		}
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	if err := waitForProvider(provider, waitSeconds); err != nil {
		return err
	}
	if err := configureAgent(agent, provider, model, baseURL, workdir); err != nil {
		return err
	}

	if hasSession(socket, session) {
		return nil
	}

	launchCmd, err := launchCommand(agent)
	if err != nil {
		return err
	}
	cmd := withPath("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", workdir, launchCmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	if agent == "kilo" {
		if err := enableKiloRemote(socket, session); err != nil {
			return fmt.Errorf("enabling remote for %s: %w", session, err)
		}
	}
	return nil
}

// StopAgentmux is the instance unit's ExecStop.
func StopAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	_ = withPath("tmux", "-L", socket, "kill-session", "-t", session).Run()
	return nil
}

// UpdateAgentmux is `agentmux session update --instance NAME` for zero/
// opencode: checks for a new version and restarts the session only if it
// changed or the session isn't running, matching
// backends/agentmux/rc-update.sh. Platform-specific (agentmux_linux.go /
// agentmux_darwin.go): Linux runs as root and needs runas to drop to the
// instance's run user plus systemctl to restart; macOS runs as the
// instance's own user already and restarts by calling StopAgentmux/
// RunAgentmux directly, with no service manager involved — see
// claudecode_darwin.go's updateClaudeCode for why that's necessary rather
// than just re-kickstarting the LaunchAgent.
func UpdateAgentmux(name string) error {
	return updateAgentmux(name)
}

// enableKiloRemote sends the /remote slash command to a freshly-started
// kilo TUI session — kilo's own equivalent of Claude Code's
// --remote-control launch flag, except it has no such flag; /remote is a
// runtime command, confirmed by driving the TUI directly and checking its
// own logs for the resulting kilosessions.ai relay connection. Only called
// right after creating a brand new session (RunAgentmux's hasSession
// early-return skips this on every idempotent re-check), matching where
// claude-code's --remote-control is passed at that same creation point.
func enableKiloRemote(socket, session string) error {
	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }
	if err := waitForPaneText(tmux, socket, session, kiloReadyMarker, kiloReadyPollInterval, kiloReadyTimeout); err != nil {
		return fmt.Errorf("waiting for %s to become interactive before enabling remote: %w", session, err)
	}
	time.Sleep(500 * time.Millisecond) // let input handling finish mounting right after its first paint
	if err := tmux("-L", socket, "send-keys", "-t", session, "/remote").Run(); err != nil {
		return fmt.Errorf("sending /remote to %s: %w", session, err)
	}
	// The command palette's fuzzy filter/selection updates asynchronously
	// after each keystroke; sending Enter in the same send-keys call as
	// the text races that update and can land before /remote is actually
	// selected, leaving the palette open with the text typed but nothing
	// chosen (confirmed empirically: same-call Enter silently no-opped).
	time.Sleep(500 * time.Millisecond)
	if err := tmux("-L", socket, "send-keys", "-t", session, "Enter").Run(); err != nil {
		return fmt.Errorf("submitting /remote to %s: %w", session, err)
	}
	return nil
}

// waitForPaneText polls session's pane content until it contains marker,
// or returns an error once timeout elapses.
func waitForPaneText(tmux func(args ...string) *exec.Cmd, socket, session, marker string, pollInterval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
		if err != nil {
			return fmt.Errorf("capturing pane: %w", err)
		}
		if strings.Contains(string(out), marker) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %q", timeout, marker)
		}
		time.Sleep(pollInterval)
	}
}

func waitForProvider(provider string, waitSeconds int) error {
	if provider != "ollama" {
		return nil
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for {
		if withPath("ollama", "list").Run() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ollama is not reachable after %ds; start ollama and re-run", waitSeconds)
		}
		time.Sleep(2 * time.Second)
	}
}

func configureAgent(agent, provider, model, baseURL, workdir string) error {
	switch agent {
	case "zero":
		return writeZeroConfig(provider, model, baseURL, workdir)
	case "opencode":
		return writeOpencodeConfig(provider, model, baseURL, workdir)
	case "kilo":
		return writeKiloCodeConfig(provider, model, baseURL, workdir)
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
}

func launchCommand(agent string) (string, error) {
	switch agent {
	case "zero", "opencode", "kilo":
		return agent, nil
	default:
		return "", fmt.Errorf("unsupported agent: %s", agent)
	}
}

func writeZeroConfig(provider, model, baseURL, workdir string) error {
	configDir := filepath.Join(workdir, ".zero")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	doc := map[string]any{
		"activeProvider": provider,
		"providers": []map[string]any{
			{
				"name":          provider,
				"provider_kind": "openai-compatible",
				"catalogID":     provider,
				"baseURL":       baseURL,
				"apiFormat":     "chat-completions",
				"model":         model,
			},
		},
	}
	if err := writeJSONAtomic(filepath.Join(configDir, "config.json"), doc); err != nil {
		return err
	}
	cmd := withPath("zero", "providers", "check", provider, "--connectivity")
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zero providers check: %w: %s", err, out)
	}
	return nil
}

func writeOpencodeConfig(provider, model, baseURL, workdir string) error {
	doc := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   provider + "/" + model,
		"provider": map[string]any{
			provider: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": provider,
				"options": map[string]any{
					"baseURL": baseURL,
				},
				"models": map[string]any{
					model: map[string]any{
						"name": model,
					},
				},
			},
		},
	}
	return writeJSONAtomic(filepath.Join(workdir, "opencode.json"), doc)
}

// writeKiloCodeConfig mirrors writeOpencodeConfig: Kilo CLI (`kilo`, from
// the @kilocode/cli npm package) is a fork of opencode and shares its
// config schema, just under its own project-level file name/$schema URL.
func writeKiloCodeConfig(provider, model, baseURL, workdir string) error {
	doc := map[string]any{
		"$schema": "https://app.kilo.ai/config.json",
		"model":   provider + "/" + model,
		"provider": map[string]any{
			provider: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": provider,
				"options": map[string]any{
					"baseURL": baseURL,
				},
				"models": map[string]any{
					model: map[string]any{
						"name": model,
					},
				},
			},
		},
	}
	return writeJSONAtomic(filepath.Join(workdir, "kilo.json"), doc)
}

func writeJSONAtomic(path string, doc any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
