// Package claudeauth drives Claude Code's interactive OAuth login from a
// browser-less host: `claude auth login` prints an authorize URL and waits
// on a "Paste code here" prompt, which an operator completes on another
// computer and pastes back. The PTY wiring lives in the CLI
// (daemon/cmd/agentmux/auth_cmd.go); this package holds command
// construction plus the output scanners both sides share, kept here so they
// are unit-testable without a real claude binary.
package claudeauth

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// LoginMethodArgs maps -method to claude's own flags: --claudeai is claude's
// default subscription flow (passed explicitly so a claude-side default
// change can't silently switch an account to Console billing), --console is
// the API-billing alternative from `claude auth login --help`.
func LoginMethodArgs(method string) ([]string, error) {
	switch method {
	case "", "claudeai":
		return []string{"--claudeai"}, nil
	case "console":
		return []string{"--console"}, nil
	default:
		return nil, fmt.Errorf("unknown login method %q (want claudeai or console)", method)
	}
}

// commandForUser builds a claude invocation as runUser: privilege-dropped
// via runas when root (the daemon/update-timer context), same-user
// otherwise. A non-root caller asking for a different user gets a clear
// error instead of a child that fails opaquely at exec.
func commandForUser(runUser, name string, args ...string) (*exec.Cmd, error) {
	if runUser == "" {
		return nil, fmt.Errorf("run user is required")
	}
	if os.Geteuid() != 0 {
		cur, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("resolving current user: %w", err)
		}
		if runUser != cur.Username {
			return nil, fmt.Errorf("cannot act as user %q while running as %q; run this command as %s", runUser, cur.Username, runUser)
		}
		return runas.CurrentUserCommand(name, args...), nil
	}
	return runas.Command(runUser, name, args...), nil
}

// StartLogin builds `claude auth login` as runUser with a headless-safe
// environment: BROWSER is neutralized so claude skips its doomed
// open-a-local-browser attempt and goes straight to printing the URL (it
// falls back to that anyway, this just makes it deterministic across
// hosts), TERM is ensured for the PTY the caller runs this under, and any
// ANTHROPIC_API_KEY/AUTH_TOKEN is stripped so the subscription OAuth flow
// actually runs instead of a key-based path with nothing to log in.
func StartLogin(runUser, method string) (*exec.Cmd, error) {
	flags, err := LoginMethodArgs(method)
	if err != nil {
		return nil, err
	}
	cmd, err := commandForUser(runUser, "claude", append([]string{"auth", "login"}, flags...)...)
	if err != nil {
		return nil, err
	}
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	env := stripEnvPrefix(cmd.Env, "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=")
	env = setEnv(env, "BROWSER=true")
	env = ensureEnv(env, "TERM=xterm-256color")
	cmd.Env = env
	return cmd, nil
}

// CheckLoggedIn runs `claude auth status --json` as runUser and reports the
// parsed result. A logged-out claude exits non-zero but still prints valid
// {"loggedIn":false,...} (confirmed live), so a parseable body is trusted
// over the exit code; only unparseable output is an error.
func CheckLoggedIn(runUser string) (loggedIn bool, authMethod string, err error) {
	cmd, err := commandForUser(runUser, "claude", "auth", "status", "--json")
	if err != nil {
		return false, "", err
	}
	if cmd.Err != nil {
		return false, "", cmd.Err
	}
	out, runErr := cmd.Output()
	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if jsonErr := json.Unmarshal(out, &status); jsonErr != nil {
		if runErr != nil {
			return false, "", fmt.Errorf("claude auth status: %w: %s", runErr, strings.TrimSpace(string(out)))
		}
		return false, "", fmt.Errorf("parsing claude auth status: %w", jsonErr)
	}
	return status.LoggedIn, status.AuthMethod, nil
}

// loginURLPattern matches the authorize URL `claude auth login` prints. It
// stops at whitespace, ESC, and BEL so the OSC-8 hyperlink wrapper claude
// emits under a PTY (ESC]8;;<url>BEL<label>...) can't leak escape bytes
// into the extracted URL.
var loginURLPattern = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?[^\s\x1b\x07]*`)

// ExtractLoginURL returns the first authorize URL in claude's output, or ""
// when none has been printed yet.
func ExtractLoginURL(output string) string {
	return loginURLPattern.FindString(output)
}

// RedactLoginURLs replaces authorize URLs with a placeholder for anything
// shown after a failure (log tails, error reports): the query carries
// single-use PKCE state, not long-lived secrets, but there is no reason to
// print it.
func RedactLoginURLs(s string) string {
	return loginURLPattern.ReplaceAllString(s, "https://claude.com/cai/oauth/authorize?[redacted]")
}

// PromptForCode reports whether claude is sitting on its code prompt.
func PromptForCode(output string) bool {
	return strings.Contains(output, "Paste code here")
}

// InvalidCodeReported reports whether claude rejected a pasted code (it
// stays on the prompt afterward, so the caller should invite another paste,
// not give up — confirmed live: two bad codes in a row re-prompt).
func InvalidCodeReported(output string) bool {
	return strings.Contains(output, "Invalid code")
}

// LoginSuccessful reports whether claude printed its success line.
func LoginSuccessful(output string) bool {
	return strings.Contains(output, "Login successful")
}

func stripEnvPrefix(env []string, prefixes ...string) []string {
	out := env[:0]
	for _, e := range env {
		keep := true
		for _, p := range prefixes {
			if strings.HasPrefix(e, p) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}

func setEnv(env []string, kv string) []string {
	key := kv[:strings.IndexByte(kv, '=')]
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, kv)
}

func ensureEnv(env []string, kv string) []string {
	key := kv[:strings.IndexByte(kv, '=')]
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return env
		}
	}
	return append(env, kv)
}
