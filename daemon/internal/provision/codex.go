package provision

import (
	"fmt"
	"os/exec"
	"strings"
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
