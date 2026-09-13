package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAmpLaunchArgs(t *testing.T) {
	got := ampLaunchArgs("kartography")
	want := []string{"--no-tui", "--runner-id", "kartography", "--remote-control-terminal"}
	if len(got) != len(want) {
		t.Fatalf("ampLaunchArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ampLaunchArgs = %v, want %v", got, want)
		}
	}
}

func TestAmpRunnerIDFor(t *testing.T) {
	// The provisioner's recorded id wins and is returned unchanged.
	id, err := ampRunnerIDFor("data-import", map[string]string{"AGENTMUX_AMP_RUNNER_ID": "data-import"})
	if err != nil {
		t.Fatalf("ampRunnerIDFor: %v", err)
	}
	if id != "data-import" {
		t.Errorf("ampRunnerIDFor with a recorded id = %q, want %q", id, "data-import")
	}

	// A registry that predates the field falls back to the instance name,
	// sanitized the same way the provisioner would have sanitized it.
	id, err = ampRunnerIDFor("my_project.v2", map[string]string{})
	if err != nil {
		t.Fatalf("ampRunnerIDFor (fallback): %v", err)
	}
	if id != "my-project-v2" {
		t.Errorf("ampRunnerIDFor with no recorded id = %q, want %q", id, "my-project-v2")
	}

	// A hand-edited registry value that isn't a valid hostname is corrected
	// rather than handed to amp as-is.
	id, err = ampRunnerIDFor("probe", map[string]string{"AGENTMUX_AMP_RUNNER_ID": "Hand_Edited.Value"})
	if err != nil {
		t.Fatalf("ampRunnerIDFor (hand-edited): %v", err)
	}
	if id != "hand-edited-value" {
		t.Errorf("ampRunnerIDFor with a hand-edited id = %q, want %q", id, "hand-edited-value")
	}
}

// TestRunAmpLaunchesTheDocumentedCommand pins the actual tmux invocation:
// the runner's working directory, its tmux session/socket, and the exact amp
// argv from ampcode.com/docs/cli/runners.
func TestRunAmpLaunchesTheDocumentedCommand(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	registryFile := "" +
		"AGENTMUX_INSTANCE_NAME=probe\n" +
		"AGENTMUX_AGENT=amp\n" +
		"AGENTMUX_AMP_RUNNER_ID=probe\n" +
		"AGENTMUX_TMUX_SESSION_NAME=probe\n" +
		"AGENTMUX_WORKDIR=" + workdir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte(registryFile), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	prevWithPath := withPath
	withPath = func(name string, args ...string) *exec.Cmd {
		if name != "tmux" {
			t.Fatalf("withPath called with unexpected command %q", name)
		}
		calls = append(calls, args)
		for _, a := range args {
			if a == "has-session" {
				return exec.Command("false") // not running yet
			}
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { withPath = prevWithPath })

	if err := RunAmp("probe"); err != nil {
		t.Fatalf("RunAmp: %v", err)
	}

	var launch []string
	for _, args := range calls {
		for _, a := range args {
			if a == "new-session" {
				launch = args
			}
		}
	}
	if launch == nil {
		t.Fatalf("RunAmp never issued a tmux new-session; calls: %v", calls)
	}
	want := []string{
		"-L", "agentmux-probe", "new-session", "-d", "-s", "probe", "-c", workdir,
		"amp", "--no-tui", "--runner-id", "probe", "--remote-control-terminal",
	}
	if strings.Join(launch, " ") != strings.Join(want, " ") {
		t.Errorf("RunAmp launched\n  %v\nwant\n  %v", launch, want)
	}
}

// TestRunAmpLeavesARunningSessionAlone is the behavioural half of the
// deliberate decision not to build a reconnect self-heal for amp (see the
// AmpPaneRemoteConnected comment in amp.go): a live session must be a strict
// no-op, with no capture-pane and above all no send-keys.
func TestRunAmpLeavesARunningSessionAlone(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	registryFile := "AGENTMUX_AGENT=amp\nAGENTMUX_TMUX_SESSION_NAME=probe\nAGENTMUX_WORKDIR=" + workdir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte(registryFile), 0o644); err != nil {
		t.Fatal(err)
	}

	prevWithPath := withPath
	withPath = func(name string, args ...string) *exec.Cmd {
		for _, a := range args {
			switch a {
			case "has-session":
				return exec.Command("true") // already running
			case "send-keys", "new-session", "capture-pane":
				t.Fatalf("RunAmp issued %q against an already-running session: %v", a, args)
			}
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { withPath = prevWithPath })

	if err := RunAmp("probe"); err != nil {
		t.Fatalf("RunAmp: %v", err)
	}
}

func TestAmpPaneAwaitingLogin(t *testing.T) {
	// Captured verbatim from a real `amp --no-tui --runner-id ...
	// --remote-control-terminal` pane on a host with no amp credentials
	// (amp 0.0.1789300838-gde32db).
	awaiting := []string{
		"No API key found. Starting login flow...\nWould you like to log in to Amp? [(y)es, (n)o]: ",
		"No API key found. Starting login flow...\nTo log in, visit:\n\nhttps://auth.ampcode.com/device?user_code=DPFX-VRKF\n\nWaiting for confirmation in the browser...",
		"No API key found. Starting login flow...\nWould you like to log in to Amp? [(y)es, (n)o]: n\nLogin cancelled. Run the command again to retry.",
	}
	for _, pane := range awaiting {
		if !AmpPaneAwaitingLogin(pane) {
			t.Errorf("AmpPaneAwaitingLogin(%q) = false, want true", pane)
		}
	}

	healthy := []string{
		"",
		"$ amp --no-tui --runner-id probe --remote-control-terminal\n",
		"waiting for threads\n",
	}
	for _, pane := range healthy {
		if AmpPaneAwaitingLogin(pane) {
			t.Errorf("AmpPaneAwaitingLogin(%q) = true, want false", pane)
		}
	}
}

func TestAmpUpdateChanged(t *testing.T) {
	cases := []struct {
		name           string
		out            string
		wantChanged    bool
		wantRecognized bool
	}{
		{
			// Captured verbatim: the porcelain line is last, behind chatter.
			name:           "no update needed behind chatter",
			out:            "Checking for updates...\n✓ Amp is already up to date on version 0.0.1789300838-gde32db (released 1h ago)\nno update needed\n",
			wantChanged:    false,
			wantRecognized: true,
		},
		{
			name:           "updated",
			out:            "Checking for updates...\nupdated 0.0.1789400000-gabcdef\n",
			wantChanged:    true,
			wantRecognized: true,
		},
		{
			// A future wording change must degrade into "assume nothing
			// changed", not into a nightly restart of every amp instance.
			name:           "unrecognized",
			out:            "Checking for updates...\nall good\n",
			wantChanged:    false,
			wantRecognized: false,
		},
		{
			name:           "empty",
			out:            "",
			wantChanged:    false,
			wantRecognized: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed, recognized := ampUpdateChanged(tc.out)
			if changed != tc.wantChanged || recognized != tc.wantRecognized {
				t.Errorf("ampUpdateChanged(%q) = (%v, %v), want (%v, %v)", tc.out, changed, recognized, tc.wantChanged, tc.wantRecognized)
			}
		})
	}
}

// TestAmpVersionIDIgnoresTheRelativeTimestamp is the reason amp doesn't
// reuse the other families' whole-output version comparison: amp --version
// embeds a relative "released ... ago" phrase that changes on its own with
// no update whatsoever, which would look like a version change every night.
func TestAmpVersionIDIgnoresTheRelativeTimestamp(t *testing.T) {
	before := "0.0.1789300838-gde32db (released 2026-09-13T12:00:38.000Z, 59m ago)\n"
	after := "0.0.1789300838-gde32db (released 2026-09-13T12:00:38.000Z, 1h ago)\n"
	if ampVersionID(before) != ampVersionID(after) {
		t.Errorf("ampVersionID treated a drifting relative timestamp as a version change: %q vs %q", ampVersionID(before), ampVersionID(after))
	}
	if got, want := ampVersionID(after), "0.0.1789300838-gde32db"; got != want {
		t.Errorf("ampVersionID = %q, want %q", got, want)
	}
	// `amp version` (the subcommand) adds a second line; the first still wins.
	if got, want := ampVersionID("0.0.1-gabc (released x, 1h ago)\nnpm package: @ampcode/cli\n"), "0.0.1-gabc"; got != want {
		t.Errorf("ampVersionID (multi-line) = %q, want %q", got, want)
	}
	if got := ampVersionID("\n\n"); got != "" {
		t.Errorf("ampVersionID(blank) = %q, want \"\"", got)
	}
}

func TestCollaborationIsNeverDeliveredToAmp(t *testing.T) {
	if collaborationSupported("amp") {
		t.Error("collaborationSupported(amp) = true, want false — amp's --no-tui runner has no prompt to deliver into")
	}
	for _, agent := range []string{"claude-code", "zero", "opencode", "kilo", ""} {
		if !collaborationSupported(agent) {
			t.Errorf("collaborationSupported(%q) = false, want true", agent)
		}
	}

	// Even a pane that would clear every generic safety check for another
	// agent must not be considered deliverable for amp.
	const idlePane = "amp runner ready\nnothing to see here\n"
	if collaborationPaneSafe("amp", idlePane) {
		t.Error("collaborationPaneSafe(amp, idle pane) = true, want false")
	}
	if !collaborationPaneSafe("zero", idlePane) {
		t.Error("collaborationPaneSafe(zero, idle pane) = false, want true (guard must be amp-specific)")
	}
}
