package provision

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// defaultCodexInstance is the instance name used when none is given and
// there is no workdir to derive one from.
const defaultCodexInstance = "codex"

// codexLoginProblemVia reports "" when `codex login status` succeeds, or why
// the login can't be confirmed. It only ever reads that command's one-line
// verdict; credentials are never touched.
func codexLoginProblemVia(cmd *exec.Cmd) string {
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	return fmt.Sprintf("codex does not appear to be logged in (`codex login status` failed: %v: %s)", err, firstLine(string(out), 200))
}

// rejectUnsupportedCodexOptions refuses options that only make sense for
// other agents. Model is the one agent-specific knob codex takes (a free
// string handed to `codex -m`).
func rejectUnsupportedCodexOptions(opts Options) error {
	var bad []string
	if opts.Provider != "" {
		bad = append(bad, "provider")
	}
	if opts.BaseURL != "" {
		bad = append(bad, "base URL")
	}
	if opts.APIKeyEnv != "" {
		bad = append(bad, "API key env")
	}
	if opts.CompactOnUpdate != "" {
		bad = append(bad, "compact-on-update")
	}
	if opts.ResumeSessionID != "" {
		bad = append(bad, "resume session id")
	}
	if len(bad) > 0 {
		return fmt.Errorf("codex does not take: %s", strings.Join(bad, ", "))
	}
	return nil
}

// CodexLoginStatus reports whether runUser is logged in to codex, from
// `codex login status` only: the auth files under CODEX_HOME are never read.
// The command's output is classified, not returned (an API-key login prints
// a key fragment), so nothing secret can reach a caller. method is "chatgpt",
// "api-key" or "unknown"; a non-zero exit is "not logged in", while a
// missing binary or unknown user is an error.
func CodexLoginStatus(runUser string) (loggedIn bool, method string, err error) {
	return codexLoginStatusVia(runas.Command(runUser, "codex", "login", "status"))
}

func codexLoginStatusVia(cmd *exec.Cmd) (bool, string, error) {
	if cmd.Err != nil {
		return false, "", cmd.Err
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return false, "", nil
		}
		return false, "", runErr
	}
	low := strings.ToLower(string(out))
	switch {
	case strings.Contains(low, "api key"):
		return true, "api-key", nil
	case strings.Contains(low, "chatgpt"):
		return true, "chatgpt", nil
	}
	return true, "unknown", nil
}
