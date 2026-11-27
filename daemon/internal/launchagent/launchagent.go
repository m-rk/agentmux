// Package launchagent keeps agentmux's macOS LaunchAgents stable across
// binary updates (AMUX-143).
//
// A job whose ProgramArguments start with the agentmux binary is tied to
// that binary's code signature. agentmux is ad-hoc signed, so every update
// gives it a new identity: launchd refuses to spawn the old registration
// (exit 78, AMUX-125) and Background Task Management treats each
// re-registration as a new login item, with one "App Background Activity"
// notification per job. Instead every plist starts the binary through
// /bin/sh, whose identity only changes with macOS itself, and a plist is
// only booted out and back in when its content changes.
//
// The package is platform neutral so the logic is tested everywhere; only
// its callers are darwin-gated.
package launchagent

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The launcher: `/bin/sh -c 'exec "$0" "$@"' BIN ARGS...` replaces the
// shell with BIN, keeping the pid launchd watches and every argument as is.
const (
	Shell      = "/bin/sh"
	ShellFlag  = "-c"
	ExecScript = `exec "$0" "$@"`
)

// LauncherXML is the launcher's ProgramArguments entries, at the indent the
// plist templates use. It goes straight after `<array>`, before the binary.
const LauncherXML = "        <string>" + Shell + "</string>\n" +
	"        <string>" + ShellFlag + "</string>\n" +
	"        <string>" + ExecScript + "</string>\n"

// Args is the launcher argv for bin and its arguments.
func Args(bin string, args ...string) []string {
	return append([]string{Shell, ShellFlag, ExecScript, bin}, args...)
}

// Launched reports whether argv starts with the launcher.
func Launched(argv []string) bool {
	return len(argv) > 3 && argv[0] == Shell && argv[1] == ShellFlag && argv[2] == ExecScript
}

// ProgramArguments returns a plist's ProgramArguments strings, or nil when
// it has none or does not parse.
func ProgramArguments(plist []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(plist))
	dec.Strict = false
	var key string
	inArgs := false
	var args []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var k string
				if dec.DecodeElement(&k, &t) != nil {
					return nil
				}
				key = strings.TrimSpace(k)
			case "array":
				if key == "ProgramArguments" {
					inArgs = true
				}
			case "string":
				var s string
				if dec.DecodeElement(&s, &t) != nil {
					return nil
				}
				if inArgs {
					args = append(args, s)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" && inArgs {
				return args
			}
		}
	}
}

// StringValue returns the <string> value right after <key>name</key> in a
// plist, such as StandardErrorPath, or "" when there is none.
func StringValue(plist []byte, name string) string {
	dec := xml.NewDecoder(bytes.NewReader(plist))
	dec.Strict = false
	afterKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		t, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if t.Name.Local == "key" {
			var k string
			if dec.DecodeElement(&k, &t) != nil {
				return ""
			}
			afterKey = strings.TrimSpace(k) == name
			continue
		}
		if afterKey && t.Name.Local == "string" {
			var v string
			if dec.DecodeElement(&v, &t) != nil {
				return ""
			}
			return strings.TrimSpace(v)
		}
		afterKey = false
	}
}

// RunsAgentmuxDirectly reports whether a plist starts the agentmux binary
// itself rather than through the launcher.
func RunsAgentmuxDirectly(plist []byte) bool {
	argv := ProgramArguments(plist)
	return len(argv) > 0 && filepath.Base(argv[0]) == "agentmux"
}

var programArrayStart = []byte("<key>ProgramArguments</key>")

// Wrap rewrites a plist that runs the agentmux binary directly so it goes
// through the launcher. It returns the plist unchanged and false for any
// other plist, including one already wrapped.
func Wrap(plist []byte) ([]byte, bool) {
	if !RunsAgentmuxDirectly(plist) {
		return plist, false
	}
	i := bytes.Index(plist, programArrayStart)
	if i < 0 {
		return plist, false
	}
	j := bytes.Index(plist[i:], []byte("<array>"))
	if j < 0 {
		return plist, false
	}
	at := i + j + len("<array>")
	if at < len(plist) && plist[at] == '\n' {
		at++
	}
	out := make([]byte, 0, len(plist)+len(LauncherXML))
	out = append(out, plist[:at]...)
	out = append(out, LauncherXML...)
	out = append(out, plist[at:]...)
	return out, true
}

// LoadedProgram returns the "program = " line of `launchctl print` output:
// the executable launchd spawns for the loaded job, or "" if it is absent.
func LoadedProgram(printOut string) string {
	for _, line := range strings.Split(printOut, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "program = "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// PinnedToAgentmux reports whether `launchctl print` output shows the job
// loaded to spawn the agentmux binary itself, which ties it to that
// binary's signature: such a job needs re-registering after a swap.
func PinnedToAgentmux(printOut string) bool {
	return filepath.Base(LoadedProgram(printOut)) == "agentmux"
}

// Launchctl runs launchd's CLI and returns its combined output.
type Launchctl func(args ...string) (string, error)

// Run is the real launchctl.
func Run(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

// Label is the job label of a LaunchAgent plist named <label>.plist.
func Label(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".plist")
}

// Apply writes content to path and registers it in domain, unless path
// already holds exactly content and the job is loaded through the launcher
// (or, for a plist that does not run agentmux, loaded at all): then nothing
// is touched and Apply returns false. A changed plist is written, booted out
// (if loaded) and bootstrapped, and Apply returns true. Bootstrapping a
// RunAtLoad job starts it; callers that want an unchanged running job on
// the new binary kickstart it themselves.
func Apply(launchctl Launchctl, domain, path string, content []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		if out, err := launchctl("print", domain+"/"+Label(path)); err == nil && !PinnedToAgentmux(out) {
			return false, nil
		}
	}
	if err := WriteIfChanged(path, content); err != nil {
		return true, err
	}
	_, _ = launchctl("bootout", domain, path) // may not be loaded
	if out, err := launchctl("bootstrap", domain, path); err != nil {
		return true, fmt.Errorf("launchctl bootstrap %s: %w: %s", path, err, strings.TrimSpace(out))
	}
	return true, nil
}

// WriteIfChanged writes content to path only when it differs from what is
// there, so an unchanged plist keeps its file untouched.
func WriteIfChanged(path string, content []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return nil
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
