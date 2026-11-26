package launchagent

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const legacyPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.agentmux.alpha</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/agentmux/bin/agentmux</string>
        <string>session</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
`

// The same job as the current templates render it.
var launchedPlist = strings.Replace(legacyPlist, "    <array>\n", "    <array>\n"+LauncherXML, 1)

func TestLauncherArgs(t *testing.T) {
	argv := Args("/x/agentmux", "daemon", "run")
	want := []string{"/bin/sh", "-c", `exec "$0" "$@"`, "/x/agentmux", "daemon", "run"}
	if strings.Join(argv, "|") != strings.Join(want, "|") {
		t.Fatalf("Args = %q, want %q", argv, want)
	}
	if !Launched(argv) || Launched(argv[3:]) {
		t.Error("Launched misreads the launcher")
	}
}

// The launcher hands the binary its arguments untouched, spaces and quotes
// included, and replaces the shell so launchd keeps watching the same pid.
func TestLauncherExecsBinaryWithArgs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "a b", "agentmux")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"pid=$$\"\nfor a in \"$@\"; do echo \"[$a]\"; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := Args(bin, "it's", "two words", `$HOME`)
	var out strings.Builder
	cmd := exec.Command(argv[0], argv[1:]...) // as launchd runs it: no shell of ours
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("pid=%d\n[it's]\n[two words]\n[$HOME]\n", cmd.Process.Pid); out.String() != want {
		t.Errorf("binary printed %q, want %q", out.String(), want)
	}
}

func TestProgramArgumentsAndWrap(t *testing.T) {
	if got := ProgramArguments([]byte(legacyPlist)); strings.Join(got, " ") != "/opt/agentmux/bin/agentmux session run" {
		t.Fatalf("ProgramArguments = %q", got)
	}
	if !RunsAgentmuxDirectly([]byte(legacyPlist)) {
		t.Fatal("legacy plist should run agentmux directly")
	}
	wrapped, ok := Wrap([]byte(legacyPlist))
	if !ok {
		t.Fatal("legacy plist was not wrapped")
	}
	if string(wrapped) != launchedPlist {
		t.Errorf("wrapped plist differs from the template form:\n%s", wrapped)
	}
	if err := xml.Unmarshal(wrapped, new(any)); err != nil {
		t.Fatalf("wrapped plist is not valid XML: %v", err)
	}
	if argv := ProgramArguments(wrapped); !Launched(argv) || argv[3] != "/opt/agentmux/bin/agentmux" {
		t.Errorf("wrapped ProgramArguments = %q", argv)
	}
	if again, ok := Wrap(wrapped); ok || string(again) != string(wrapped) {
		t.Error("wrapping twice changed the plist")
	}
	script := strings.Replace(legacyPlist, "/opt/agentmux/bin/agentmux", "/opt/backend/run.sh", 1)
	if out, ok := Wrap([]byte(script)); ok || string(out) != script {
		t.Error("a plist that does not run agentmux must be left alone")
	}
	if ProgramArguments([]byte("not a plist")) != nil {
		t.Error("garbage should have no ProgramArguments")
	}
}

const printLauncher = "gui/501/com.agentmux.alpha = {\n\tactive count = 1\n\tpath = /var/empty/Library/LaunchAgents/com.agentmux.alpha.plist\n\tstate = running\n\n\tprogram = /bin/sh\n\targuments = {\n\t\t/bin/sh\n\t}\n}\n"

var printPinned = strings.Replace(printLauncher, "program = /bin/sh", "program = /opt/agentmux/bin/agentmux", 1)

func TestLoadedProgram(t *testing.T) {
	if got := LoadedProgram(printLauncher); got != "/bin/sh" {
		t.Errorf("LoadedProgram = %q", got)
	}
	if PinnedToAgentmux(printLauncher) || !PinnedToAgentmux(printPinned) || PinnedToAgentmux("") {
		t.Error("PinnedToAgentmux misreads launchctl print")
	}
}

type fakeLaunchctl struct {
	loaded map[string]string // label -> print output
	calls  []string
}

func (f *fakeLaunchctl) run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == "print" {
		label := args[1][strings.LastIndex(args[1], "/")+1:]
		if out, ok := f.loaded[label]; ok {
			return out, nil
		}
		return "Could not find service", errors.New("exit status 113")
	}
	return "", nil
}

// An update that leaves a plist's content alone must not touch launchd's
// registration of it: each re-registration is a new "App Background
// Activity" notification on macOS (AMUX-143).
func TestApplyLeavesUnchangedLoadedJobAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "com.agentmux.alpha.plist")
	if err := os.WriteFile(path, []byte(launchedPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeLaunchctl{loaded: map[string]string{"com.agentmux.alpha": printLauncher}}
	changed, err := Apply(f.run, "gui/501", path, []byte(launchedPlist))
	if err != nil || changed {
		t.Fatalf("Apply = %v, %v; want false, nil", changed, err)
	}
	if strings.Join(f.calls, ";") != "print gui/501/com.agentmux.alpha" {
		t.Errorf("calls = %q, want only a print", f.calls)
	}
}

func TestApplyReregistersWhenNeeded(t *testing.T) {
	cases := []struct {
		name, onDisk string
		loaded       map[string]string
	}{
		{"content changed", legacyPlist, map[string]string{"com.agentmux.alpha": printPinned}},
		{"not loaded", launchedPlist, map[string]string{}},
		{"loaded pinned to the binary", launchedPlist, map[string]string{"com.agentmux.alpha": printPinned}},
		{"new plist", "", map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "com.agentmux.alpha.plist")
			if c.onDisk != "" {
				if err := os.WriteFile(path, []byte(c.onDisk), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f := &fakeLaunchctl{loaded: c.loaded}
			changed, err := Apply(f.run, "gui/501", path, []byte(launchedPlist))
			if err != nil || !changed {
				t.Fatalf("Apply = %v, %v; want true, nil", changed, err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != launchedPlist {
				t.Error("plist was not written")
			}
			calls := strings.Join(f.calls, ";")
			if !strings.Contains(calls, "bootout gui/501 "+path) || !strings.HasSuffix(calls, "bootstrap gui/501 "+path) {
				t.Errorf("calls = %q, want bootout then bootstrap", f.calls)
			}
		})
	}
}

func TestApplyReportsBootstrapFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "com.agentmux.alpha.plist")
	failing := func(args ...string) (string, error) {
		if args[0] == "bootstrap" {
			return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
		}
		return "", errors.New("not loaded")
	}
	if _, err := Apply(failing, "gui/501", path, []byte(launchedPlist)); err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteIfChangedKeepsUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(path, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteIfChanged(path, []byte("same")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Error("an unchanged file was rewritten")
	}
	if err := WriteIfChanged(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("content = %q", got)
	}
}
