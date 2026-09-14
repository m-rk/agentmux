package session

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestClaudeRemoteConnected(t *testing.T) {
	const connectedWithModeHint = `  /remote-control is active · Continue here, on your phone, or at
  https://claude.ai/code/session_example
                                                               ● high · /effort
────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  user ⚠ git:(main) ↑12  ✏️  +0/-0                                           /rc
  🤖 Sonnet 5  🪟  5%
  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents
`
	const indicatorOutsideFooter = `/rc
footer one
footer two
footer three
footer four
footer five
footer six
`
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"connected footer", "some output\n  workdir  \U0001F4DD +0/-0                                                 /rc\n", true},
		{"connected above mode hint", connectedWithModeHint, true},
		{"disconnected, no indicator", "some output\n❯ \n", false},
		{"menu open hides the footer", "   Enter to select · Esc to continue\n", false},
		{"indicator outside footer window", indicatorOutsideFooter, false},
		{"lookalike footer text", "status /rc-old\nmodel\nauto mode\n", false},
		// 2.1.248+ moved /rc out of the footer. The "/remote-control is
		// active" body text confirms the connection instead.
		{"connected via body indicator (no footer /rc)", "/remote-control is active · Continue here, on your phone, or at\nhttps://claude.ai/code/session_example\n\n\n───\n❯\n───\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents\n", true},
		{"body indicator absent, no footer /rc", "Sonnet 5 · Claude Pro\nworkdir\n\n\n\n───\n❯\n───\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmux := fakeTmuxCapture(tc.pane, nil)
			if got := claudeRemoteConnected(tmux, "sock", "sess"); got != tc.want {
				t.Errorf("claudeRemoteConnected(%q) = %v, want %v", tc.pane, got, tc.want)
			}
		})
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

 /Users/mark/hostel-harley-mini

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
