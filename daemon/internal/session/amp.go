package session

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/provision"
)

// ampLoginPromptMarkers are literal lines the Amp CLI prints when it has no
// stored API key, confirmed live against amp 0.0.1789300838-gde32db by
// launching `amp --no-tui --runner-id <id> --remote-control-terminal` in a
// tmux pane on a host with no amp credentials:
//
//	No API key found. Starting login flow...
//	Would you like to log in to Amp? [(y)es, (n)o]:
//
// and, when stdin isn't a terminal it can prompt on, the device-code
// variant:
//
//	To log in, visit:
//	...
//	Waiting for confirmation in the browser...
//
// A pane sitting on any of these is wedged: nobody is going to answer it, and
// the runner will never register. provision's own preflight
// (ampAuthProblemVia) is what stops such an instance being created in the
// first place; this is the runtime counterpart, reported by the host-wide
// doctor so an account that gets logged out later is visible rather than
// silently dead.
var ampLoginPromptMarkers = []string{
	"No API key found",
	"Would you like to log in to Amp?",
	"To log in, visit:",
	"Login cancelled. Run the command again to retry.",
}

// AmpPaneRemoteConnected deliberately does not exist. Claude Code has
// ensureClaudeRemoteControl because of one *specific, confirmed* upstream
// websocket bug (anthropics/claude-code#31853) with a known, idempotent
// one-keystroke fix; kilo has enableKiloRemote because /remote is a
// runtime-only toggle with a confirmed footer badge to check it against. amp
// has neither: --remote-control-terminal is a launch flag, not a toggle, and
// nothing in amp's docs, its --help output, or the CLI itself exposes a
// connected/disconnected indicator that a pane scrape could key off — the
// headless runner prints no status chrome at all once it is running. Inventing
// a reconnect heuristic for a failure mode nobody has observed would mean
// typing unsolicited keystrokes into a pane on every tick to fix a problem
// that may not exist, which is strictly worse than doing nothing. So the amp
// tick is a pure liveness check: restart the session if it's gone, otherwise
// leave it completely alone. Revisit only with real evidence of a real drop.

// AmpPaneAwaitingLogin reports whether a captured amp pane is stuck on the
// CLI's interactive login flow. Exported for the host-wide doctor so its
// health check and this package use one definition.
func AmpPaneAwaitingLogin(pane string) bool {
	for _, marker := range ampLoginPromptMarkers {
		if strings.Contains(pane, marker) {
			return true
		}
	}
	return false
}

// ampLaunchArgs is the documented headless-runner invocation from
// ampcode.com/docs/cli/runners, confirmed present in `amp --help` for
// 0.0.1789300838-gde32db:
//
//   - --no-tui              start a headless runner that serves remotely
//     created threads for the current directory
//   - --runner-id <id>      stable identity for this machine/checkout
//   - --remote-control-terminal  let ampcode.com drive this runner's terminal
//
// Returned as an argv slice, not a shell string, so nothing here needs
// quoting (see RunClaudeCode's matching note).
func ampLaunchArgs(runnerID string) []string {
	return []string{"--no-tui", "--runner-id", runnerID, "--remote-control-terminal"}
}

// ampRunnerIDFor resolves the runner ID for an instance: the one the
// provisioner recorded, or — for a registry that predates the field or was
// hand-written — one derived from the instance name. Either way the value
// goes through provision.AmpRunnerID, which is idempotent, so a recorded ID
// is returned unchanged while a hand-edited one that isn't a valid hostname
// is corrected rather than handed to amp as-is.
func ampRunnerIDFor(name string, fields map[string]string) (string, error) {
	id := fields["AGENTMUX_AMP_RUNNER_ID"]
	if id == "" {
		id = name
	}
	return provision.AmpRunnerID(id)
}

// RunAmp is `agentmux session run --instance NAME` for the amp agent:
// idempotently ensures the instance's tmux session is running amp's headless
// runner. Runs as the instance's target user already (the unit's User=
// directive).
//
// Unlike RunClaudeCode and RunAgentmux, this never types into the pane — see
// the AmpPaneRemoteConnected comment above — so it needs no withTmuxInputLock
// and can't race the nightly update or a Discord collab delivery. A session
// that is already up is left strictly untouched, including one wedged at the
// login prompt: restarting that would not produce credentials it doesn't
// have, it would only churn the unit. The doctor reports it instead.
func RunAmp(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	workdir := fields["AGENTMUX_WORKDIR"]

	runnerID, err := ampRunnerIDFor(name, fields)
	if err != nil {
		return fmt.Errorf("resolving amp runner id for %s: %w", name, err)
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	if hasSession(socket, session) {
		return nil
	}

	tmuxArgs := append([]string{"-L", socket, "new-session", "-d", "-s", session, "-c", workdir, "amp"}, ampLaunchArgs(runnerID)...)
	if out, err := withPath("tmux", tmuxArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	return nil
}

// StopAmp is the instance unit's ExecStop. It mirrors StopAgentmux's
// wait-for-gone rather than claude-code's fire-and-forget kill: the
// provisioner re-provisions an existing instance by stopping it and then
// immediately rewriting units and starting a replacement, and a still-exiting
// process overlapping that window is the shape of bug StopAgentmux's doc
// comment records for kilo. amp has no agentmux-written per-instance config
// for a stale process to clobber, so this is prudence rather than a fix for a
// confirmed amp failure — the cost is at most stopSessionGrace.
func StopAmp(name string) error {
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

// UpdateAmp is `agentmux session update --instance NAME` for amp: updates
// the Amp CLI and restarts the session only if the CLI actually changed or
// the session isn't running. Platform-specific (amp_linux.go /
// amp_darwin.go), matching the other two families: Linux runs as root and
// needs runas plus systemctl; macOS runs as the instance's own user and
// restarts by calling StopAmp/RunAmp directly.
func UpdateAmp(name string) error {
	return updateAmp(name)
}

// ampUpdateChanged parses `amp update --porcelain`'s machine-readable
// result. amp documents exactly two outputs ("updated <version>" or "no
// update needed") but prints human chatter alongside them — confirmed live,
// a no-op update emits "Checking for updates..." and a "✓ Amp is already up
// to date on version ..." line before the porcelain line — so this scans for
// a recognized line from the bottom rather than assuming the output is only
// the porcelain result.
//
// recognized is false when neither form appears at all. Callers treat that
// as "assume nothing changed" rather than "assume it did": amp has no
// resume, so an unnecessary restart drops whatever thread the runner is
// serving, and a future change to amp's porcelain wording should degrade
// into doing nothing rather than into a nightly restart of every amp
// instance on the host.
func ampUpdateChanged(out string) (changed bool, recognized bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "no update needed":
			return false, true
		case strings.HasPrefix(line, "updated "):
			return true, true
		}
	}
	return false, false
}

// ampVersionID extracts just the version token from `amp --version`, whose
// full output is e.g.
//
//	0.0.1789300838-gde32db (released 2026-09-13T12:00:38.000Z, 1h ago)
//
// The trailing parenthetical carries a *relative* timestamp that drifts on
// its own ("59m ago" became "1h ago" between two runs an hour apart with no
// update in between), so the whole-output string comparison the other two
// families use for change detection (agentVersion in agentmux_linux.go)
// would report a spurious change here. Only the leading token is stable.
func ampVersionID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}
