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
	var kiloEnv []string
	if agent == "kilo" {
		// Sets it on the tmux *server's* environment (this new-session call
		// only runs when no server/session exists yet for this socket, see
		// the hasSession check above), which every pane subsequently
		// launched under this socket — including a later `--session <id>`
		// resume — inherits. Passed via Cmd.Env rather than a tmux -e/
		// setenv CLI argument deliberately: CLI args land in
		// /proc/<pid>/cmdline, which is world-readable, unlike
		// /proc/<pid>/environ.
		env, err := readKiloExtraEnv()
		if err != nil {
			return fmt.Errorf("reading local kilo env overlay: %w", err)
		}
		kiloEnv = env
		isolatedEnv, err := kiloInstanceXDGEnv(name)
		if err != nil {
			return fmt.Errorf("preparing isolated kilo data for %s: %w", name, err)
		}
		// Keep these entries last so a shared XDG_DATA_HOME/XDG_STATE_HOME in
		// kilo-env cannot accidentally collapse prepared instances back onto
		// the same database and state directory. Config and cache remain
		// untouched and can still be shared deliberately.
		kiloEnv = append(kiloEnv, isolatedEnv...)

		id, err := latestKiloSessionID(workdir, kiloEnv)
		if err != nil {
			return fmt.Errorf("checking for an existing kilo session in %s: %w", workdir, err)
		}
		if id == "" {
			displayName := provision.DisplayNameFor(fields["AGENTMUX_RUN_USER"], workdir)
			newID, err := seedKiloSession(workdir, kiloEnv, displayName)
			if err != nil {
				return fmt.Errorf("seeding initial kilo session in %s: %w", workdir, err)
			}
			id = newID
		}
		launchCmd = "kilo --session " + id
	}
	cmd := withPath("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", workdir, launchCmd)
	cmd.Env = append(cmd.Env, kiloEnv...)
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

// stopSessionPollInterval/stopSessionTimeout/stopSessionGrace bound how
// long StopAgentmux waits for the killed session to actually be gone —
// see StopAgentmux's own doc comment for why this wait exists at all.
// vars (not consts) so tests can shrink them instead of actually
// sleeping for real timeouts.
var (
	stopSessionPollInterval = 100 * time.Millisecond
	stopSessionTimeout      = 5 * time.Second
	stopSessionGrace        = 1 * time.Second
)

// StopAgentmux is the instance unit's ExecStop.
//
// `tmux kill-session` sends a hangup to the pane's process and tears down
// tmux's own session bookkeeping essentially immediately — but it does not
// wait for that process to actually finish exiting, and a well-behaved
// agent CLI (kilo confirmed live) does async work in its own shutdown
// handler: flushing its last-used model/session state to its local
// database before it actually calls exit(). If the caller — ExecStart on
// a plain `systemctl restart`, or provision.createAgentmux re-provisioning
// an existing instance — proceeds to write updated config and launch a
// new process the moment this function returns, that still-exiting old
// process can flush its *stale* in-memory state to disk a moment later,
// silently clobbering the update that was just written. Confirmed live:
// a provider/model change reproducibly failed to take effect this way,
// and only reliably stuck once an explicit wait for the old process to be
// fully gone was added before writing anything new.
//
// tmux itself gives no synchronous "wait for this session's process to
// actually exit" primitive, so this polls has-session (which does at
// least confirm tmux's own bookkeeping is gone) and then adds a short
// fixed grace period for the signaled process's own async shutdown to
// finish — a heuristic, not a guarantee, but one that's already proven
// out the actual failure mode above end-to-end.
func StopAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	_ = withPath("tmux", "-L", socket, "kill-session", "-t", session).Run()

	deadline := time.Now().Add(stopSessionTimeout)
	for hasSession(socket, session) && time.Now().Before(deadline) {
		time.Sleep(stopSessionPollInterval)
	}
	time.Sleep(stopSessionGrace)
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

// kiloRemoteIndicator is the footer badge kilo renders once a session is
// actually connected to the remote relay.
const kiloRemoteIndicator = "◆ Remote"

// kiloRemoteEntryMarker is the command palette's fixed help text for its
// /remote entry. Waited on before submitting Enter so the toggle isn't
// blind-submitted before the palette's async fuzzy-filter has actually
// rendered /remote as the top (selected) candidate — a fixed delay alone
// was confirmed live to sometimes race this and silently no-op: the
// palette stayed open with "/remote" typed but Enter landed before
// selection settled.
const kiloRemoteEntryMarker = "Enable or disable remote session relay"

// KiloPaneReady and KiloPaneRemoteConnected expose the same stable footer
// markers used during startup so the host-wide doctor does not invent a
// second, subtly different definition of Kilo health.
func KiloPaneReady(pane string) bool {
	return strings.Contains(pane, kiloReadyMarker)
}

func KiloPaneRemoteConnected(pane string) bool {
	return strings.Contains(pane, kiloRemoteIndicator)
}

// enableKiloRemote ensures session ends up connected to kilo's remote
// relay — kilo's own equivalent of Claude Code's --remote-control launch
// flag, except it has no such flag; /remote is a runtime-only toggle.
// Confirmed live: launching via `kilo --session <id>` (every kilo
// instance agentmux creates now goes through this path — see
// seedKiloSession) auto-connects remote by default already, no /remote
// command needed. Because it's a toggle, unconditionally sending /remote
// (the original approach) silently *disconnects* an already-connected
// session instead of connecting it — confirmed live via kilo's own
// "Remote disabled" toast appearing right after an unconditional toggle
// on a session that had auto-connected on resume. So: wait for the TUI,
// check whether it's already connected, and only drive the toggle if it
// isn't — keeping the keystroke automation (and its timing fragility)
// off the common path, as a fallback for a machine/config where the
// auto-connect-on-resume behavior doesn't hold.
func enableKiloRemote(socket, session string) error {
	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }
	if err := waitForPaneText(tmux, socket, session, kiloReadyMarker, kiloReadyPollInterval, kiloReadyTimeout); err != nil {
		return fmt.Errorf("waiting for %s to become interactive before enabling remote: %w", session, err)
	}
	time.Sleep(500 * time.Millisecond) // let input handling finish mounting right after its first paint

	out, err := tmux("-L", socket, "capture-pane", "-p", "-t", session).Output()
	if err != nil {
		return fmt.Errorf("checking remote status for %s: %w", session, err)
	}
	if strings.Contains(string(out), kiloRemoteIndicator) {
		return nil // already connected (kilo --session auto-connects on resume)
	}

	if err := tmux("-L", socket, "send-keys", "-t", session, "/remote").Run(); err != nil {
		return fmt.Errorf("sending /remote to %s: %w", session, err)
	}
	if err := waitForPaneText(tmux, socket, session, kiloRemoteEntryMarker, kiloReadyPollInterval, kiloReadyTimeout); err != nil {
		return fmt.Errorf("waiting for the /remote command palette entry to render in %s: %w", session, err)
	}
	time.Sleep(400 * time.Millisecond) // palette text and its selection state are separate render passes
	if err := tmux("-L", socket, "send-keys", "-t", session, "Enter").Run(); err != nil {
		return fmt.Errorf("submitting /remote to %s: %w", session, err)
	}

	if err := waitForPaneText(tmux, socket, session, kiloRemoteIndicator, kiloReadyPollInterval, 5*time.Second); err != nil {
		return fmt.Errorf("/remote was submitted for %s but the remote indicator never appeared: %w", session, err)
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
func latestKiloSessionID(workdir string, env []string) (string, error) {
	cmd := withPath("kilo", "session", "list", "--format", "json")
	cmd.Dir = workdir
	cmd.Env = append(cmd.Env, env...)
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
	// kilo records a session's directory fully resolved (symlinks
	// followed) — e.g. a workdir under macOS's /tmp, which is itself a
	// symlink to /private/tmp, comes back as /private/tmp/..., not
	// /tmp/.... Comparing against the literal workdir string missed
	// every session for a symlinked path; resolve both sides the same
	// way before comparing. Falls back to the literal path if it
	// doesn't exist (nothing to resolve) or resolution fails.
	resolvedWorkdir := workdir
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil {
		resolvedWorkdir = resolved
	}
	best, bestUpdated := "", int64(0)
	for _, s := range sessions {
		if s.Directory != workdir && s.Directory != resolvedWorkdir {
			continue
		}
		if best == "" || s.Updated > bestUpdated {
			best, bestUpdated = s.ID, s.Updated
		}
	}
	return best, nil
}

// kiloSeedTimeout bounds how long the one-shot `kilo run` used by
// seedKiloSession to create a fresh session is allowed to take: a full
// round trip to the model, not just a TUI paint, so wider than
// kiloReadyTimeout.
const kiloSeedTimeout = 45 * time.Second

// seedKiloSession creates workdir's first kilo session via a single
// non-interactive `kilo run <message> --title <title>` invocation inside
// a throwaway tmux session, rather than the previous approach (launch a
// bare interactive `kilo`, type the seed message via send-keys, then
// patch the title into kilo's private sqlite schema afterward via `kilo
// db`). That approach raced kilo's own async title handling and was
// confirmed live to sometimes lose the custom title back to the raw
// seed-message text after a later relaunch. `--title` is kilo's own
// supported, natively-synced title mechanism — confirmed live to land
// immediately and survive a relaunch, unlike the private-schema write.
// Must run inside a real pty: a bare exec.Command with no tty attached
// was confirmed to hang indefinitely rather than complete, hence the
// throwaway tmux session rather than running the command directly.
// Returns the new session's ID so the caller can attach the actual
// long-running pane to it via `kilo --session <id>` — the same rendering
// path (full TUI, correct agent/model) already used for resumes, since
// `kilo run --interactive` was confirmed to render a different, more
// limited UI that didn't even pick up the configured model.
func seedKiloSession(workdir string, env []string, title string) (string, error) {
	runCmd := fmt.Sprintf("kilo run %s --title %s", shellQuote(kiloSeedMessage), shellQuote(title))

	socket := "agentmux-kilo-seed-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	tmux := func(args ...string) *exec.Cmd { return withPath("tmux", args...) }
	defer tmux("-L", socket, "kill-server").Run()

	newSession := tmux("-L", socket, "new-session", "-d", "-s", "seed", "-c", workdir, runCmd)
	newSession.Env = append(newSession.Env, env...)
	if out, err := newSession.CombinedOutput(); err != nil {
		return "", fmt.Errorf("starting seed session: %w: %s", err, out)
	}

	// tmux new-session -d returns as soon as it forks, not once the pane
	// is actually registered — checking has-session immediately can race
	// that and see "not found" before the session ever existed, which
	// looks identical to "already finished." Only treat its absence as
	// completion once it's been observed alive at least once.
	deadline := time.Now().Add(kiloSeedTimeout)
	observedAlive := false
	for {
		alive := tmux("-L", socket, "has-session", "-t", "seed").Run() == nil
		if alive {
			observedAlive = true
		} else if observedAlive {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for the seed session to finish", kiloSeedTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The one-shot `kilo run` process exiting doesn't guarantee kilo's own
	// session-list visibility has caught up yet (the same class of lag
	// waitForKiloSessionID used to guard against for the old send-keys
	// flow) — poll briefly rather than failing on the first empty check.
	seedDeadline := time.Now().Add(10 * time.Second)
	for {
		id, err := latestKiloSessionID(workdir, env)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
		if time.Now().After(seedDeadline) {
			return "", fmt.Errorf("kilo run exited but no session was found for %s after %s", workdir, 10*time.Second)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// shellQuote wraps s in single quotes for safe use inside a shell command
// string built by hand (tmux new-session's shell-command argument is
// passed to the pane's shell, not exec'd as an argv array), escaping any
// embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
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

// readKiloExtraEnv reads an optional local, never-committed dotenv-style
// file at ~/.config/agentmux/kilo-env (NAME=VALUE per line, an optional
// leading "export ", blank lines and lines starting with "#" ignored)
// and returns its entries ready to append to an exec.Cmd's Env.
//
// kilo merges a personal global config (~/.config/kilo/) into every
// project automatically, so defining an extra provider — a private/paid
// gateway, say — needs no agentmux involvement at all... except that kilo
// flatly refuses any "{env:VAR}" reference in *project*-level config
// ("environment references are not allowed in project config", confirmed
// via `kilo config check`), while allowing it in the global config an
// interactive terminal session would already have from its normal shell
// profile. agentmux's tmux-launched kilo processes are non-interactive
// and don't source a shell profile, so without this, a provider needing
// an API key via "{env:VAR}" in the global config would work in a
// terminal but silently fail (empty/missing key) inside every
// agentmux-managed instance. This file exists solely to close that one
// gap — it carries no opinion about what provider or key it holds,
// agentmux's own source never needs to know. A missing file is not an
// error, just "nothing to add" — most boxes won't have one.
func readKiloExtraEnv() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "agentmux", "kilo-env"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var env []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if line == "" || strings.HasPrefix(line, "#") || !ok {
			continue
		}
		// Shell-quoted values (the file is also meant to be `source`-able
		// from a shell profile, where quoting matters) aren't unquoted by
		// Go's exec.Cmd the way a shell would — confirmed the hard way: an
		// unstripped surrounding '"' landed inside the actual env var
		// value, corrupting an API key enough that kilo crashed outright on
		// startup rather than just failing auth gracefully.
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		env = append(env, name+"="+value)
	}
	return env, nil
}

const kiloXDGIsolationReadyFile = ".xdg-isolation-ready"

// kiloInstanceXDGEnv returns per-instance data and state roots after an
// operator has explicitly marked the instance ready for cutover. Existing
// Kilo installations share credentials and session history in the default
// XDG directories, so enabling isolation merely because a new agentmux binary
// was installed would make the next unattended restart look like an empty,
// logged-out installation. The marker keeps deployment and data migration as
// separate operations: no marker means preserve the legacy environment.
//
// The isolation root is derived from agentmux's instance identity rather than
// a caller-selected workdir. Custom workdirs may be arbitrary source
// checkouts; putting OAuth material and a live SQLite database inside one
// would expose secrets to project tooling and make accidental commits much
// too easy.
func kiloInstanceXDGEnv(instance string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return kiloInstanceXDGEnvForHome(instance, home)
}

func kiloInstanceXDGEnvForHome(instance, home string) ([]string, error) {
	if instance == "" || instance == "." || instance == ".." || filepath.Base(instance) != instance {
		return nil, fmt.Errorf("invalid instance name %q for kilo isolation", instance)
	}
	root := filepath.Join(home, ".agentmux", instance, ".kilo-home")
	marker := filepath.Join(root, kiloXDGIsolationReadyFile)
	markerInfo, err := os.Lstat(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !markerInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("readiness marker %s must be a regular file", marker)
	}

	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	for _, dir := range []string{root, dataDir, stateDir} {
		if err := ensurePrivateKiloDir(dir); err != nil {
			return nil, err
		}
	}
	return []string{
		"XDG_DATA_HOME=" + dataDir,
		"XDG_STATE_HOME=" + stateDir,
	}, nil
}

func ensurePrivateKiloDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("isolated kilo path %s must be a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
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
