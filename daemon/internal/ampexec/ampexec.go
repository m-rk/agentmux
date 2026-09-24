// Package ampexec runs one bounded `amp -x` (execute mode) call for a
// caller that wants amp as an alternative to `claude -p` for a small,
// untrusted-data analysis step — currently threadwatch's nightly review
// (internal/threadwatch/review_amp.go) and the daily doctor's escalation
// (internal/dailycheck/amp.go). See docs/thread-watch.md's "amp backend"
// section and docs/doctor.md's "-checker amp" for the operator-facing
// picture; this file is the shared mechanics both build on.
//
// Every call here creates a real amp thread visible on ampcode.com — that
// is intentional, not a leak to guard against.
package ampexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

// CommandFactory creates the (possibly privilege-dropped) amp process. Same
// shape as dailycheck.CommandFactory / threadwatch.CommandFactory
// (identical underlying function type, so a runas-wrapped factory built for
// one assigns straight into the other) so CLI wiring can reuse the exact
// factory it already builds for the claude escalation.
type CommandFactory func(context.Context, string, ...string) *exec.Cmd

// Config configures one Run call.
type Config struct {
	// Executor selects where the thread runs. "local" (the default, used
	// when this is empty) runs amp directly on this host with every tool
	// disabled via a generated settings file — see Run's doc comment for
	// why only this executor can make that guarantee. "runner:<id>" runs
	// on an already-started `amp --no-tui` runner (`amp runner list`
	// shows what's available); tools cannot be disabled from here, so a
	// caller choosing this executor must log a startup warning about the
	// prompt-injection risk (RunnerWarning below builds that message).
	Executor string
	// Workdir is the local executor's working directory (its -x thread's
	// cwd). Ignored for a runner executor, which has its own RunnerDir.
	Workdir string
	// RunnerDir is the --runner-dir passed to a runner executor, selecting
	// which of that runner's served directories the thread runs in. Empty
	// leaves it at the runner's own default.
	RunnerDir string
	// Mode is an optional amp -m/--mode override (low|medium|high|ultra,
	// or a plugin mode). Empty uses amp's own default.
	Mode string
	// Label is the -l/--label applied to the thread this call creates.
	Label string
	// APIKey, when non-empty, is passed to the child as AMP_API_KEY in its
	// environment only — never in argv — so it never appears in `ps` or
	// process-listing output. Empty relies on amp's own stored `amp login`
	// session.
	APIKey string
	// Binary overrides the amp executable name/path; defaults to "amp".
	Binary string
	// Owner, when set, chowns the generated tool-disabling settings file
	// to this user after creating it. Required whenever CommandFactory
	// drops privilege (a root process spawning amp as another user via
	// runas): the settings file is created 0600 by the (often root)
	// calling process, so without a chown the user amp actually runs as
	// could not read its own settings file.
	Owner *user.User
}

const defaultBinary = "amp"

// Run sends message to amp in execute mode (-x) over stdin — never argv, so
// nothing in message (which may embed untrusted transcript/pane excerpts)
// ends up visible in `ps` — and returns amp's last assistant message text
// verbatim. message is expected to already demand a JSON-only reply, the
// same contract dailycheck/threadwatch's claude escalations get for free
// from `claude -p --json-schema`; amp has no equivalent flag, so callers
// must spell the schema out in message itself, and use ExtractJSON to strip
// an optional code fence from what comes back before parsing it.
//
// For the (default) "local" executor, Run writes a temporary, 0600 amp
// settings file that disables every tool two independent ways —
// amp.tools.disable: ["*"] and a catch-all amp.permissions reject rule —
// and passes it via --settings-file, removing it before returning. This is
// the only tool-safety guarantee this package can make, and the reason
// "local" is the safe default: the whole point of running amp over
// untrusted excerpts is that it must not be able to act on anything hiding
// in them.
//
// For a "runner:<id>" executor, tools cannot be disabled from here at all —
// that runner's own settings decide what it can call, and Run has no way
// to inspect or override them remotely. Run does not warn about this on
// every call (a bounded job like the nightly review or the doctor runs
// once per invocation, so per-call logging would just be noise); a caller
// choosing a runner executor should log RunnerWarning's message once at
// its own startup instead.
func Run(ctx context.Context, command CommandFactory, cfg Config, message string) (string, error) {
	if command == nil {
		return "", fmt.Errorf("no amp command factory configured")
	}
	binary := cfg.Binary
	if binary == "" {
		binary = defaultBinary
	}
	executor := cfg.Executor
	if executor == "" {
		executor = "local"
	}

	args := []string{"-x", "--executor", executor, "--no-color"}
	if cfg.Mode != "" {
		args = append(args, "-m", cfg.Mode)
	}
	if cfg.Label != "" {
		args = append(args, "-l", cfg.Label)
	}

	isLocal := executor == "local"
	if isLocal {
		settingsPath, cleanup, err := writeToolDisableSettings(cfg.Owner)
		if err != nil {
			return "", fmt.Errorf("preparing amp settings file: %w", err)
		}
		defer cleanup()
		args = append(args, "--settings-file", settingsPath)
	} else if cfg.RunnerDir != "" {
		args = append(args, "--runner-dir", cfg.RunnerDir)
	}

	cmd := command(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(message)
	if isLocal && cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	if cfg.APIKey != "" {
		// cmd.Environ(), not cmd.Env directly: when the factory leaves Env
		// nil (inherit the current process's environment), append(nil, x)
		// would replace that inherited environment with just x instead of
		// adding to it. Environ() returns the effective environment either
		// way, so appending to its result always adds rather than clobbers.
		cmd.Env = append(cmd.Environ(), "AMP_API_KEY="+cfg.APIKey)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RunnerWarning returns the one-line prompt-injection warning a caller
// should log once at startup when executor selects a runner (see Run's doc
// comment), and false when it selects the local executor (or is empty,
// which defaults to local) and there is nothing to warn about.
func RunnerWarning(executor string) (warning string, isRunner bool) {
	if executor == "" || executor == "local" {
		return "", false
	}
	return fmt.Sprintf("amp executor %q cannot have its tools disabled by agentmux; untrusted transcript/pane excerpts will reach a thread that keeps whatever tools that runner's own settings allow — verify the runner's own permissions before relying on this", executor), true
}

// toolDisableSettings is the amp settings.json shape Run writes for the
// local executor: amp.tools.disable blocks every built-in and MCP tool by
// name/glob, and the amp.permissions catch-all reject rule blocks anything
// a future tool might not be caught by that glob on. Both were verified
// against a real amp CLI (see docs/thread-watch.md's amp section) rather
// than assumed from documentation alone.
type toolDisableSettings struct {
	ToolsDisable []string             `json:"amp.tools.disable"`
	Permissions  []toolPermissionRule `json:"amp.permissions"`
}

type toolPermissionRule struct {
	Tool   string `json:"tool"`
	Action string `json:"action"`
}

func toolDisableSettingsJSON() ([]byte, error) {
	return json.MarshalIndent(toolDisableSettings{
		ToolsDisable: []string{"*"},
		Permissions:  []toolPermissionRule{{Tool: "*", Action: "reject"}},
	}, "", "  ")
}

// writeToolDisableSettings creates a 0600 temporary amp settings file that
// disables every tool (see toolDisableSettings) and returns its path and a
// cleanup func that removes it; the caller must defer cleanup(). When owner
// is set, the file is chowned to it — see Config.Owner's doc comment for
// why that matters whenever the amp process itself runs as a different,
// unprivileged user.
func writeToolDisableSettings(owner *user.User) (path string, cleanup func(), err error) {
	data, err := toolDisableSettingsJSON()
	if err != nil {
		return "", nil, err
	}

	f, err := os.CreateTemp("", "agentmux-amp-settings-*.json")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { os.Remove(path) }

	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	if owner != nil {
		uid, err := strconv.Atoi(owner.Uid)
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("invalid UID %q for user %q: %w", owner.Uid, owner.Username, err)
		}
		gid, err := strconv.Atoi(owner.Gid)
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("invalid GID %q for user %q: %w", owner.Gid, owner.Username, err)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("chowning %s to %s: %w", path, owner.Username, err)
		}
	}
	return path, cleanup, nil
}

// ExtractJSON pulls the JSON object out of amp's last-assistant-message
// text for a message that asked for "JSON only, optionally inside a
// ```json code fence" (see Run's doc comment on why the contract has to be
// spelled out in the prompt rather than enforced by a flag). It strips one
// leading/trailing fence when present and trims surrounding whitespace
// either way; it does not validate that what remains is actually JSON —
// that is the caller's json.Unmarshal's job, and a failure there is a
// parse failure the caller falls back on, exactly like a schema-
// non-conformant claude reply.
func ExtractJSON(text string) []byte {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "```") {
		return []byte(s)
	}
	s = strings.TrimPrefix(s, "```")
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		// Drop the fence's opening line (a language tag like "json", or
		// nothing) along with the fence marker itself.
		s = s[nl+1:]
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return []byte(strings.TrimSpace(s))
}
