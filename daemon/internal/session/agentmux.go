package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/provision"
)

const (
	// kiloReadyMarker is text kilo's TUI only renders once it's actually
	// interactive (the footer command-hint bar) — not present during the
	// cold-boot window (tmux pane exists but node/kilo hasn't drawn its
	// first frame yet), which idle-stability detection would wrongly
	// treat as "settled": a pane that hasn't started rendering is just as
	// unchanging as one that's finished. Confirmed against a live cold
	// start: the pane can sit blank/on the splash for several seconds
	// (model-list fetch, indexing) before this text ever appears. Not
	// "Ask anything" (the empty-input placeholder) — confirmed that text
	// is absent when resuming a session via `kilo --session <id>` that
	// already has history (see latestKiloSessionID), which left
	// enableKiloRemote timing out on every resumed launch even though the
	// TUI was, in fact, already interactive.
	kiloReadyMarker       = "ctrl+p commands"
	kiloReadyPollInterval = 500 * time.Millisecond
	kiloReadyTimeout      = 30 * time.Second
)

// kiloSeedMessage is sent once, right after remote is enabled on a
// freshly created kilo session — and only on that session's true first
// launch, never on a later resume (see latestKiloSessionID). Kilo has no
// separate "register this session" step — a session doesn't exist (and
// so isn't visible in the mobile/web app's session list) until its first
// message is sent, confirmed via `kilo session list` staying empty for an
// instance that had never been typed into despite /remote already being
// connected. For a remote-only user with no terminal to type that first
// message from, agentmux has to send it instead, or the instance would
// sit invisible forever.
const kiloSeedMessage = "This is an automated startup check-in from agentmux, just to register this session in your session list. Please reply with a short acknowledgement and take no other action."

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
	resumingKilo := false
	if agent == "kilo" {
		id, err := latestKiloSessionID(workdir)
		if err != nil {
			return fmt.Errorf("checking for an existing kilo session in %s: %w", workdir, err)
		}
		if id != "" {
			launchCmd = "kilo --session " + id
			resumingKilo = true
		}
	}
	cmd := withPath("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", workdir, launchCmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	if agent == "kilo" {
		if err := enableKiloRemote(socket, session); err != nil {
			return fmt.Errorf("enabling remote for %s: %w", session, err)
		}
		if !resumingKilo {
			if err := seedKiloSession(socket, session); err != nil {
				return fmt.Errorf("seeding initial session for %s: %w", session, err)
			}
			titleNewKiloSession(workdir, fields["AGENTMUX_RUN_USER"])
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

// latestKiloSessionID returns the id of the most recently updated kilo
// session already recorded for workdir, or "" if none exists yet. Without
// this, every tmux-session recreation — including a plain nightly `kilo
// upgrade` restart, which doesn't imply any conversation was actually
// lost — launched a bare `kilo` and re-ran seedKiloSession, abandoning the
// previous session and creating a brand new one in its place; confirmed
// live, this left the user's session list permanently accumulating
// duplicate "Agentmux startup check-in" entries, one per restart, forever.
// RunAgentmux now launches `kilo --session <id>` instead when one is
// found, which resumes that exact session (verified: same session id,
// full history replayed, no new row created) rather than starting fresh.
//
// `kilo session list`'s default project-scoping is inconsistent for
// directories without a distinct git identity — confirmed empirically,
// unrelated directories can share its fallback "global" project and all
// show up together in one unscoped listing — so results here are always
// filtered by the directory field explicitly rather than trusted by scope
// alone. Deliberately omits --all: confirmed it crashes kilo outright
// ("undefined is not an object (evaluating 'J.time.updated')") when
// combined with a directory that has no sessions of its own yet, which is
// exactly the first-ever-launch case this function most needs to handle
// correctly.
func latestKiloSessionID(workdir string) (string, error) {
	cmd := withPath("kilo", "session", "list", "--format", "json")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("listing kilo sessions: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", nil // a project with zero sessions prints nothing, not "[]"
	}
	var sessions []struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
		Updated   int64  `json:"updated"`
	}
	if err := json.Unmarshal(out, &sessions); err != nil {
		return "", fmt.Errorf("parsing kilo session list: %w", err)
	}
	best, bestUpdated := "", int64(0)
	for _, s := range sessions {
		if s.Directory != workdir {
			continue
		}
		if best == "" || s.Updated > bestUpdated {
			best, bestUpdated = s.ID, s.Updated
		}
	}
	return best, nil
}

// seedKiloSession types kiloSeedMessage into a just-remote-enabled kilo
// session and submits it, the same two-send-keys-calls-with-a-pause
// pattern enableKiloRemote uses and for the same reason: submitting in
// the same call as the text is unreliable. Doesn't wait for a reply —
// submitting the message is what creates the session record; agentmux
// doesn't need the model to actually finish responding.
func seedKiloSession(socket, session string) error {
	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }
	if err := tmux("-L", socket, "send-keys", "-t", session, kiloSeedMessage).Run(); err != nil {
		return fmt.Errorf("sending seed message to %s: %w", session, err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := tmux("-L", socket, "send-keys", "-t", session, "Enter").Run(); err != nil {
		return fmt.Errorf("submitting seed message to %s: %w", session, err)
	}
	return nil
}

// titleNewKiloSession sets a freshly created kilo session's title to
// agentmux's own display-name convention (provision.DisplayNameFor —
// the same "<user>:<host> 🤹 <workdir-basename>" format claude-code
// uses), via `kilo db` — kilo's own sanctioned SQL-against-its-local-
// database tool, and the only way to write a session's title at all
// outside of `kilo run --title`, which only exists in --interactive
// mode and can't run alongside /remote (see the design doc). This
// writes straight to kilo's private, undocumented schema, and the
// resulting sync back up to the cloud/app is equally undocumented —
// confirmed working live, but only once the connected process's own
// heartbeat pushed it, a few minutes after the write, not instantly.
// Best-effort only, deliberately swallowing errors rather than
// returning them: remote and seeding are the load-bearing parts of
// this flow, and a cosmetic title must never be able to take either of
// them down, especially given kilo could change this private schema
// out from under agentmux at any point. Only ever called right after
// seedKiloSession, on a session's true first creation — never on a
// resume — so a title the user changes by hand in the app afterward is
// never clobbered.
func titleNewKiloSession(workdir, runUser string) {
	id, err := waitForKiloSessionID(workdir, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmux: not setting a kilo session title for %s: %v\n", workdir, err)
		return
	}
	title := provision.DisplayNameFor(runUser, workdir)
	if err := setKiloSessionTitle(workdir, id, title); err != nil {
		fmt.Fprintf(os.Stderr, "agentmux: not setting a kilo session title for %s: %v\n", workdir, err)
	}
}

// waitForKiloSessionID polls latestKiloSessionID until it finds a
// session for workdir or timeout elapses — seedKiloSession submits the
// message and returns without waiting for kilo to finish registering
// it, so the row isn't guaranteed to exist yet the instant it returns.
func waitForKiloSessionID(workdir string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		id, err := latestKiloSessionID(workdir)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for the seeded session to appear", timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// setKiloSessionTitle writes title into kilo's local session table for
// id via `kilo db`, which runs the given SQL directly with no
// parameterization — id is always kilo's own generated ses_-prefixed
// identifier, but title is built from a hostname/workdir basename, so
// it's escaped defensively even though neither is attacker-controlled
// in practice.
func setKiloSessionTitle(workdir, id, title string) error {
	escaped := strings.ReplaceAll(title, "'", "''")
	query := fmt.Sprintf("UPDATE session SET title = '%s' WHERE id = '%s'", escaped, id)
	cmd := withPath("kilo", "db", query)
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kilo db: %w: %s", err, out)
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
