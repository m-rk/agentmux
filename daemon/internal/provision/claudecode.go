package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// claudeJSONPath returns the on-disk credentials/config file Claude Code
// checks workspace trust against: $HOME/.claude.json, confirmed directly
// (not assumed), falling back to $HOME/.claude/.claude.json in case some
// other Claude Code version/config uses that nesting. May return a path
// that doesn't exist; preacceptWorkspaceTrust no-ops in that case (current
// Claude Code versions may keep credentials in the OS keychain with no
// on-disk JSON file at all).
func claudeJSONPath(home string) string {
	p := filepath.Join(home, ".claude.json")
	if fileExists(p) {
		return p
	}
	if alt := filepath.Join(home, ".claude", ".claude.json"); fileExists(alt) {
		return alt
	}
	return p
}

// preacceptWorkspaceTrust patches claudeJSON's
// projects[workdir].hasTrustDialogAccepted = true, natively via
// encoding/json instead of install.sh's/install-macos.sh's inline Python.
// chown, if non-nil, is applied to the rewritten file afterward — needed on
// Linux, where this runs as root writing on behalf of a different runUser;
// nil on macOS, where the process already runs as the target user and no
// ownership fixup is needed.
func preacceptWorkspaceTrust(claudeJSON, workdir string, chown func(path string) error) error {
	if !fileExists(claudeJSON) {
		return nil
	}
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	proj, _ := projects[workdir].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
		projects[workdir] = proj
	}
	if accepted, _ := proj["hasTrustDialogAccepted"].(bool); accepted {
		return nil
	}
	proj["hasTrustDialogAccepted"] = true

	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := claudeJSON + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if chown != nil {
		if err := chown(tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, claudeJSON)
}

// claudeLoginRetryDelay is how long createClaudeCode waits before checking
// login a second time. A credentials refresh or a racing `claude` process can
// make `claude auth status` report logged-out for a few seconds on a host
// that is in fact logged in; one retry rides that out.
var claudeLoginRetryDelay = 20 * time.Second

// claudeLoginProbe runs `claude auth status --json` via an
// already-configured *exec.Cmd — each OS builds that differently (a
// privilege-dropped runas.Command on Linux, a same-user
// runas.CurrentUserCommand on macOS) — and parses the JSON result. Shared so
// both platforms parse the same response shape the same way. The returned
// string says what was seen, for the error when login is not confirmed.
func claudeLoginProbe(cmd *exec.Cmd) (bool, string) {
	out, err := cmd.Output()
	if err != nil {
		detail := err.Error()
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			detail += ": " + firstLine(string(ee.Stderr), 200)
		}
		return false, "`claude auth status --json` failed (" + detail + ")"
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return false, "`claude auth status --json` printed unparseable output (" + firstLine(string(out), 200) + ")"
	}
	if !status.LoggedIn {
		return false, "`claude auth status --json` reported loggedIn=false"
	}
	return true, ""
}

// checkClaudeLogin probes login, and on failure waits claudeLoginRetryDelay
// and probes once more before giving up. newCmd builds a fresh command per
// attempt (an *exec.Cmd runs once). The error names both checks, so a
// transient failure is distinguishable from a real logout.
func checkClaudeLogin(who string, newCmd func() *exec.Cmd) error {
	ok, first := claudeLoginProbe(newCmd())
	if ok {
		return nil
	}
	time.Sleep(claudeLoginRetryDelay)
	ok, second := claudeLoginProbe(newCmd())
	if ok {
		return nil
	}
	return fmt.Errorf("Claude Code does not appear to be logged in%s: checked twice, %s apart; first: %s; second: %s; if it is logged in this may be a credentials refresh in progress, so retry; otherwise run 'claude' once%s to log in",
		who, claudeLoginRetryDelay, first, second, who)
}
