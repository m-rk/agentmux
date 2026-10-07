package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTmuxCapture returns a tmux stand-in whose capture-pane always reports
// pane, and records every send-keys invocation's trailing key args (skipping
// the "-L socket ... -t session" prefix common to all calls here).
func fakeTmuxCapture(pane string, sent *[]string) func(args ...string) *exec.Cmd {
	return func(args ...string) *exec.Cmd {
		if contains(args, "capture-pane") {
			return exec.Command("printf", "%s", pane)
		}
		if contains(args, "send-keys") {
			for i, a := range args {
				if a == "-t" && i+2 <= len(args)-1 {
					*sent = append(*sent, args[i+2:]...)
					break
				}
			}
			return exec.Command("true")
		}
		return exec.Command("true")
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestClaudeRemoteConnectedAlwaysTrue guards the current, deliberate
// fail-open contract of ClaudePaneRemoteConnected (see its doc comment):
// neither known indicator renders in claude-code 2.1.271 even for a
// genuinely connected session (confirmed live on a Linux host,
// 2026-09-15), and reporting "disconnected" in that case caused confirmed
// active harm (spurious /remote-control keystrokes into every live
// session on every idle tick, plus a false doctor alert and blocked
// Discord collaboration delivery). So every case here — including the
// realistic "no known indicator at all" pane — must return true.
func TestClaudeRemoteConnectedAlwaysTrue(t *testing.T) {
	panes := []string{
		"",
		"some output\n❯ \n",
		"   Enter to select · Esc to continue\n",
		// The exact shape of a real, genuinely-connected 2.1.271 pane: no
		// /rc, no /remote-control, anywhere.
		"Sonnet 5 · Claude Pro\nworkdir\n\n\n\n───\n❯\n───\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents\n",
		// Still recognized, in case a mixed-fleet host ever shows it again.
		"some output\n  workdir  \U0001F4DD +0/-0                                                 /rc\n",
	}
	for _, pane := range panes {
		tmux := fakeTmuxCapture(pane, nil)
		if got := claudeRemoteConnected(tmux, "sock", "sess"); got != true {
			t.Errorf("claudeRemoteConnected(%q) = %v, want true", pane, got)
		}
	}
}

func TestClaudeRemoteMenuOpen(t *testing.T) {
	const menuAboveHint = `Disconnect this session
Show QR code
Continue
Enter to select · Esc to continue
model hint
auto mode hint
`
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"menu open", "   Enter to select · Esc to continue\n", true},
		{"menu above mode hints", menuAboveHint, true},
		{"connected, no menu", "workdir /rc\n", false},
		{"disconnected, no menu", "❯ \n", false},
		{"menu text outside footer window", "Esc to continue\n1\n2\n3\n4\n5\n6\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmux := fakeTmuxCapture(tc.pane, nil)
			if got := claudeRemoteMenuOpen(tmux, "sock", "sess"); got != tc.want {
				t.Errorf("claudeRemoteMenuOpen(%q) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
}

// trustDialogPane is a real capture of Claude Code's workspace-trust screen
// (from a workdir the trust store didn't recognize after a rename), used
// below to confirm the detector and its caller's refusal to type into it.
const trustDialogPane = `
 Accessing workspace:

  /Users/dev/example-project

 Quick safety check: Is this a project you created or one you trust? (Like your
 own code, a well-known open source project, or work from your team). If not,
 take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func TestClaudeTrustDialogOpen(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"trust dialog open", trustDialogPane, true},
		{"connected, no dialog", "workdir /rc\n", false},
		{"remote control menu, not trust dialog", "   Enter to select · Esc to continue\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmux := fakeTmuxCapture(tc.pane, nil)
			if got := claudeTrustDialogOpen(tmux, "sock", "sess"); got != tc.want {
				t.Errorf("claudeTrustDialogOpen(%q) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
}

// TestEnsureClaudeRemoteControlRefusesTrustDialog guards against the actual
// bug this session diagnosed live: a workdir rename left Claude Code
// showing its workspace-trust screen (defaulted to "No, exit") instead of a
// running session, and ensureClaudeRemoteControl blindly sent
// "/remote-control" + Enter into it on every periodic tick — submitting
// that default and killing the process seconds after every restart, with
// no error anywhere. It must instead recognize the dialog and send nothing.
func TestEnsureClaudeRemoteControlRefusesTrustDialog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := withEnvDir(t)
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte("AGENTMUX_INSTANCE_NAME=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sent []string
	tmux := fakeTmuxCapture(trustDialogPane, &sent)

	if err := ensureClaudeRemoteControl(tmux, "probe", "sock", "sess"); err != nil {
		t.Fatalf("ensureClaudeRemoteControl on a trust-dialog pane: %v, want nil (skipped, not a failure)", err)
	}
	if len(sent) != 0 {
		t.Fatalf("ensureClaudeRemoteControl sent keystrokes into an open trust dialog: %v -- this would confirm its default \"No, exit\" and kill the session", sent)
	}
}

func TestDismissClaudeRemoteMenuIfOpen(t *testing.T) {
	t.Run("dismisses an open menu via Escape", func(t *testing.T) {
		var sent []string
		tmux := fakeTmuxCapture("   Enter to select · Esc to continue\n", &sent)
		dismissed, err := dismissClaudeRemoteMenuIfOpen(tmux, "sock", "sess")
		if err != nil {
			t.Fatalf("dismissClaudeRemoteMenuIfOpen: %v", err)
		}
		if !dismissed {
			t.Fatal("dismissed = false, want true when the menu is open")
		}
		if !contains(sent, "Escape") {
			t.Errorf("sent keys = %v, want to include Escape", sent)
		}
	})

	t.Run("no-op when the menu isn't open", func(t *testing.T) {
		var sent []string
		tmux := fakeTmuxCapture("workdir /rc\n", &sent)
		dismissed, err := dismissClaudeRemoteMenuIfOpen(tmux, "sock", "sess")
		if err != nil {
			t.Fatalf("dismissClaudeRemoteMenuIfOpen: %v", err)
		}
		if dismissed {
			t.Fatal("dismissed = true, want false when the menu isn't open")
		}
		if len(sent) != 0 {
			t.Errorf("sent keys = %v, want none when nothing needed dismissing", sent)
		}
	})
}

// TestClaudeLaunchArgs pins the claude command line: instances without
// allow-files must launch byte-identically to before the feature existed.
func TestClaudeLaunchArgs(t *testing.T) {
	cases := []struct {
		name     string
		resume   string
		settings string
		model    string
		effort   string
		want     []string
	}{
		{"plain", "", "", "", "", []string{"--remote-control", "disp"}},
		{"resume", "abc123", "", "", "", []string{"--remote-control", "disp", "--resume", "abc123"}},
		{"allow-files", "", "/h/s.json", "", "", []string{"--remote-control", "disp", "--settings", "/h/s.json"}},
		{"model", "", "", "opus", "", []string{"--remote-control", "disp", "--model", "opus"}},
		{"effort", "", "", "", "high", []string{"--remote-control", "disp", "--effort", "high"}},
		{"model, effort and resume", "abc", "", "opus", "high", []string{"--remote-control", "disp", "--model", "opus", "--effort", "high", "--resume", "abc"}},
		{"allow-files and resume", "abc123", "/h/s.json", "", "", []string{"--remote-control", "disp", "--resume", "abc123", "--settings", "/h/s.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeLaunchArgs("disp", tc.resume, tc.settings, tc.model, tc.effort)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("claudeLaunchArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrepareClaudeAllowSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "work")
	note := filepath.Join(home, "vault", "TASK-1 t.md")
	if err := os.MkdirAll(filepath.Dir(note), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	realNote, _ := filepath.EvalSymlinks(note)
	fields := map[string]string{"AGENTMUX_ALLOW_FILES": `["` + realNote + `"]`}

	path := prepareClaudeAllowSettings("inst", work, fields)
	if want := filepath.Join(home, ".agentmux", ".settings", "inst.claude.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v, %v; want mode 0600", info, err)
	}
	if d, _ := os.Stat(filepath.Dir(path)); d.Mode().Perm() != 0o700 {
		t.Errorf("settings dir mode = %v, want 0700", d.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "Read(/"+realNote+")") {
		t.Errorf("settings missing read rule: %s", data)
	}

	// A file that went away is skipped; with nothing left the stale
	// settings file is removed and no --settings is passed.
	os.Remove(note)
	if got := prepareClaudeAllowSettings("inst", work, fields); got != "" {
		t.Errorf("path with all files gone = %q, want empty", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale settings file still present: %v", err)
	}
	// Cleared field: same.
	if got := prepareClaudeAllowSettings("inst", work, map[string]string{}); got != "" {
		t.Errorf("path with no allow-files = %q, want empty", got)
	}
}
