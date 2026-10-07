package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/ampconfig"
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
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
// Since "one runner is now enough" (ampcode.com/news/one-runner-is-now-enough),
// a runner can also serve more than its start directory: --discover-dirs
// serves every Git checkout up to two levels beneath it (picking up new
// clones), and each --dir adds one more directory explicitly. dirs comes
// from the AGENTMUX_AMP_DIRS registry value (already split); non-absolute
// entries are skipped defensively — the provisioner rejects them up front,
// so one here means a hand-edited registry, and handing amp a relative
// path would silently serve somewhere unintended.
//
// Returned as an argv slice, not a shell string, so nothing here needs
// quoting (see RunClaudeCode's matching note).
func ampLaunchArgs(runnerID string, dirs []string, discoverDirs bool) []string {
	args := []string{"--no-tui", "--runner-id", runnerID}
	if discoverDirs {
		args = append(args, "--discover-dirs")
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			fmt.Printf("warning: skipping non-absolute amp dir %q (expand ~ to an absolute path)\n", d)
			continue
		}
		args = append(args, "--dir", d)
	}
	return append(args, "--remote-control-terminal")
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

// ampLaunchArgsFor resolves the amp runner argv for an instance from its
// registry fields, with the instance's effective mode included as -m:
// every runner process starts with the host mode (AMUX-36), the same
// guarantee sessions run gives each thread. A missing mode everywhere is
// an error, never a silent run on amp's default model.
func ampLaunchArgsFor(name string, fields map[string]string) ([]string, error) {
	host, herr := ampconfig.Load(ampconfig.DefaultPath())
	if herr != nil {
		// A broken host file is caller-visible only when it would matter:
		// with an instance override the host file is never consulted.
		if strings.TrimSpace(fields[ampconfig.EnvOverride]) == "" {
			return nil, fmt.Errorf("reading amp host config: %w", herr)
		}
		host = ampconfig.Config{}
	}
	mode, _, err := ampconfig.Require(host, fields[ampconfig.EnvOverride])
	if err != nil {
		return nil, err
	}
	runnerID, err := ampRunnerIDFor(name, fields)
	if err != nil {
		return nil, fmt.Errorf("resolving amp runner id for %s: %w", name, err)
	}
	args := ampLaunchArgs(runnerID, provision.AmpSplitDirs(fields["AGENTMUX_AMP_DIRS"]), fields["AGENTMUX_AMP_DISCOVER_DIRS"] == "1")
	return append(args, ampconfig.SpawnArgs(mode)...), nil
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

	launchArgs, err := ampLaunchArgsFor(name, fields)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	if hasSession(socket, session) {
		return nil
	}

	// The default launch is amp itself. An instance with an op env-file
	// (see openv.go) instead re-enters agentmux, which starts amp through
	// `op run` so its secrets never touch tmux, argv, or the registry.
	// Task instances get the wrapper first on PATH (see taskAmpStubArgs):
	// a bare `amp` typed in the worker's own pane carries the host mode
	// — real runs are still started by agentmux, not by the worker.
	agentCmd := append([]string{"amp"}, launchArgs...)
	if opEnvFileExists(name) {
		if err := opPreflight(); err != nil {
			return fmt.Errorf("instance %s has an op env-file (%s) but cannot start with it: %w", name, opEnvFilePath(name), err)
		}
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving current executable: %w", err)
		}
		agentCmd = []string{self, "session", "exec", "--instance", name}
	}
	tmuxArgs := append([]string{"-L", socket, "new-session", "-d", "-s", session, "-c", workdir}, taskAmpStubArgs(name, fields)...)
	tmuxArgs = append(tmuxArgs, agentCmd...)
	if out, err := withPath("tmux", tmuxArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	return nil
}

// taskAmpStubDir is the directory holding the task-session amp stub, one
// per run user under their own home: ~/.agentmux/stubs. It is created on
// demand by ensureTaskAmpStub, never shipped in the repo.
func taskAmpStubDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agentmux", "stubs")
}

// taskAmpStubRefusal is what the wrapper prints when it refuses: why a
// direct amp call can't run and what to do instead. Kept in one place
// so the wrapper script and the tests quote the same text.
const taskAmpStubRefusal = "agentmux starts amp for you; test with fakes or -dry-run"

// AllowLiveAmpEnv is the opt-in that lets a task instance run a real
// `amp -x` or `threads continue -x` (AMUX-49). Unset, the wrapper refuses:
// those calls run a model turn on a real thread and spend real money.
// Only the console or the operator sets it.
const AllowLiveAmpEnv = "AGENTMUX_ALLOW_LIVE_AMP"

// taskAmpLiveRefusal is the one line the wrapper prints and logs when it
// refuses a live execute; the script and the tests quote the same text.
const taskAmpLiveRefusal = "refusing live amp -x in a task instance (" + AllowLiveAmpEnv + "=1 to allow); tests use daemon/testdata/fakeamp or sessions run -dry-run"

// taskAmpWrapper is the amp wrapper installed first on PATH in task-*
// instances' panes (AMUX-45). A worker that runs `amp` by hand bypasses
// `sessions run`, so without this the thread starts on amp's default
// model; the wrapper adds the instance's effective mode as -m to every
// thread-creating call instead. Read-only calls (threads list/export,
// version, ...) pass through untouched, and an explicit -m/--mode is
// never doubled. With no mode the wrapper refuses rather than starting a
// thread on the default model. `-x` (also `threads continue -x`) runs a
// real model turn, so it is refused unless AGENTMUX_ALLOW_LIVE_AMP=1
// (AMUX-49); the refusal is one line on stderr and in $AGENTMUX_TASK_LOG
// (default amp-refused.log beside the wrapper).
//
// The mode rides AGENTMUX_AMP_MODE, stamped per session by taskAmpStubArgs
// from the same Require resolution the runner itself uses; the wrapper
// never parses amp.yaml. taskAmpWrapperFor fills in the mode env var
// name and the refusal text; the body has no % directives so Sprintf
// cannot mangle its %% parameter expansions.
const taskAmpWrapper = `#!/bin/sh
# agentmux task wrapper (AMUX-45): manual amp calls in a task pane get
# the host mode. Generated - do not hand-edit; RunAmp rewrites it when
# its content drifts.
SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ "${@@ALLOW_ENV@@:-}" != 1 ]; then
	for a in "$@"; do
		case "$a" in
		--) break ;;
		-x|--execute|--orb-execute|-ox)
			MSG="amp: @@LIVE_REFUSAL@@"
			echo "$MSG" >&2
			echo "$MSG" >> "${AGENTMUX_TASK_LOG:-$SELF_DIR/amp-refused.log}" 2>/dev/null
			exit 1 ;;
		esac
	done
fi
MODE="${@@MODE_ENV@@:-}"
if [ -z "$MODE" ]; then
	echo "amp: no amp mode configured (@@MODE_ENV@@ unset): @@REFUSAL@@" >&2
	exit 1
fi
REAL=""
REST="$PATH:"
while [ -n "$REST" ]; do
	d=${REST%%:*}
	REST=${REST#*:}
	[ -z "$d" ] && d="."
	if [ "$d" != "$SELF_DIR" ]; then
		if [ -x "$d/amp" ] && [ ! -d "$d/amp" ]; then
			REAL="$d/amp"
			break
		fi
	fi
done
if [ -z "$REAL" ]; then
	echo "amp: no real amp behind the wrapper on PATH" >&2
	exit 1
fi
NEEDS_MODE=0
SEEN_DASHDASH=0
PREV=""
for a in "$@"; do
	if [ "$SEEN_DASHDASH" = 0 ]; then
		case "$a" in
		-m|--mode|-m=*|--mode=*) exec "$REAL" "$@" ;;
		--) SEEN_DASHDASH=1 ;;
		-x|--execute|--orb-execute|-ox) NEEDS_MODE=1 ;;
		esac
		if [ "$PREV" = "threads" ] || [ "$PREV" = "thread" ] || [ "$PREV" = "t" ]; then
			case "$a" in
			new|n|continue|c) NEEDS_MODE=1 ;;
			esac
		fi
		if [ "$PREV" = "" ]; then
			case "$a" in
			last|l) NEEDS_MODE=1 ;;
			esac
		fi
	fi
	PREV="$a"
done
if [ "$NEEDS_MODE" = 1 ]; then
	exec "$REAL" -m "$MODE" "$@"
fi
exec "$REAL" "$@"
`

// taskAmpWrapperFor renders taskAmpWrapper with the mode env var name
// and refusal text filled in. strings.Replace, not Sprintf: the body
// carries shell %% expansions that a format string would eat.
func taskAmpWrapperFor() string {
	s := strings.ReplaceAll(taskAmpWrapper, "@@MODE_ENV@@", ampconfig.EnvOverride)
	s = strings.ReplaceAll(s, "@@ALLOW_ENV@@", AllowLiveAmpEnv)
	s = strings.ReplaceAll(s, "@@LIVE_REFUSAL@@", taskAmpLiveRefusal)
	return strings.ReplaceAll(s, "@@REFUSAL@@", taskAmpStubRefusal)
}

// taskAmpStubArgs returns the tmux new-session -e pairs for task-*
// instances: the wrapper first on PATH plus the instance's effective amp
// mode for the wrapper to inject (AMUX-45). Any other instance gets
// nothing — non-task workers keep the real amp. fields is the instance's
// registry entry, whose AGENTMUX_AMP_MODE override wins over the host
// file exactly as in ampLaunchArgsFor; a missing mode everywhere means no
// pairs (and ampLaunchArgsFor refuses the runner itself), never a silent
// run on amp's default model.
func taskAmpStubArgs(name string, fields map[string]string) []string {
	if !strings.HasPrefix(name, liveguard.TaskPrefix) {
		return nil
	}
	dir := taskAmpStubDir()
	if dir == "" {
		return nil
	}
	host, herr := ampconfig.Load(ampconfig.DefaultPath())
	if herr != nil {
		if strings.TrimSpace(fields[ampconfig.EnvOverride]) == "" {
			fmt.Fprintf(os.Stderr, "%s: task amp wrapper: %v\n", name, herr)
			return nil
		}
		host = ampconfig.Config{}
	}
	mode, _, err := ampconfig.Require(host, fields[ampconfig.EnvOverride])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: task amp wrapper: %v\n", name, err)
		return nil
	}
	if err := ensureTaskAmpStub(dir); err != nil {
		fmt.Fprintf(os.Stderr, "%s: task amp wrapper: %v\n", name, err)
		return nil
	}
	if err := ensureTaskCodexStub(dir); err != nil {
		fmt.Fprintf(os.Stderr, "%s: task codex wrapper: %v\n", name, err)
	}
	return []string{"-e", "PATH=" + dir + ":$PATH", "-e", ampconfig.EnvOverride + "=" + mode}
}

// ensureTaskAmpStub writes the wrapper script into dir, rewriting it
// whenever its content drifts (so the AMUX-36 refusing stub upgrades in
// place on the next RunAmp). An identical file is left alone.
func ensureTaskAmpStub(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	script := taskAmpWrapperFor()
	path := filepath.Join(dir, "amp")
	if data, err := os.ReadFile(path); err == nil && string(data) == script {
		return nil
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
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

// pnpmNoGlobalBinDir is the pnpm error code emitted when pnpm is on PATH
// (so amp's updater picks it as the package manager) but `pnpm setup` was
// never run / PNPM_HOME is unset, leaving pnpm with no global bin
// directory. Confirmed live on a Linux host: every `amp update` — nightly
// unit and manual alike — failed with `ERR_PNPM_NO_GLOBAL_BIN_DIR` while
// `npm install -g @ampcode/cli@latest` for the same package worked fine.
const pnpmNoGlobalBinDir = "ERR_PNPM_NO_GLOBAL_BIN_DIR"

// ampUpdateTargetVersion scans `amp update` output for its "Updating to
// version <v>..." line and returns <v> (trailing dots trimmed), or "" when
// the line is absent — the caller then installs @latest. Only ever used as
// a version pin for the npm fallback with an @latest default, so a future
// wording change degrades into installing latest rather than failing.
func ampUpdateTargetVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "Updating to version "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		ver := strings.Trim(strings.TrimPrefix(line, prefix), ".")
		if fields := strings.Fields(ver); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// runAmpUpdate refreshes the amp CLI via `amp update --porcelain` and
// reports whether the CLI version changed. run executes a binary the way
// the calling platform requires (withPath on macOS, runAs on Linux); every
// call here must go through run so tests can fake the whole exchange.
//
// When amp's own updater fails only because it shelled out to a pnpm with
// no global bin directory (pnpmNoGlobalBinDir in its output), runAmpUpdate
// falls back to the equivalent `npm install -g @ampcode/cli@<version>` —
// the same package amp itself was trying to install, pinned to the version
// from its "Updating to version" line (@latest when that line is absent) —
// and determines changed by comparing `amp --version` before and after
// (via ampVersionID, so the drifting relative timestamp can't fake a
// change). Any other `amp update` failure is returned as-is with no npm
// attempted.
//
// The fallback can only fire when amp itself chose the npm-wrapper route
// (its output names the `pnpm add -g @ampcode/cli` command), so a
// curl-installed amp — where the npm route would be wrong — can never
// reach it.
func runAmpUpdate(run func(name string, args ...string) ([]byte, error)) (out []byte, changed, recognized bool, err error) {
	out, err = run("amp", "update", "--porcelain")
	if err == nil {
		changed, recognized = ampUpdateChanged(string(out))
		return out, changed, recognized, nil
	}
	if !strings.Contains(string(out), pnpmNoGlobalBinDir) {
		return out, false, false, err
	}

	version := ampUpdateTargetVersion(string(out))
	spec := "@ampcode/cli@latest"
	if version != "" {
		spec = "@ampcode/cli@" + version
	}
	before, _ := run("amp", "--version")
	npmOut, npmErr := run("npm", "install", "-g", spec)
	if npmErr != nil {
		return out, false, false, fmt.Errorf("amp update failed via misconfigured pnpm (%s; fix with `pnpm setup` or PNPM_HOME) and fallback `npm install -g %s` also failed, leaving existing session running untouched: %w: %s", pnpmNoGlobalBinDir, spec, npmErr, npmOut)
	}
	after, afterErr := run("amp", "--version")
	if afterErr != nil {
		return npmOut, false, true, fmt.Errorf("npm fallback installed %s but amp is not runnable afterward, leaving existing session running untouched: %w: %s", spec, afterErr, after)
	}
	changed = ampVersionID(string(before)) != ampVersionID(string(after))
	return npmOut, changed, true, nil
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
