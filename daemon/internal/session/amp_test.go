package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAmpLaunchArgs(t *testing.T) {
	cases := []struct {
		name     string
		runnerID string
		dirs     []string
		discover bool
		want     []string
	}{
		{
			name:     "single directory runner is unchanged",
			runnerID: "kartography",
			want:     []string{"--no-tui", "--runner-id", "kartography", "--remote-control-terminal"},
		},
		{
			name:     "discover and explicit dirs slot in before remote-control-terminal",
			runnerID: "site",
			dirs:     []string{"/srv/site"},
			discover: true,
			want:     []string{"--no-tui", "--runner-id", "site", "--discover-dirs", "--dir", "/srv/site", "--remote-control-terminal"},
		},
		{
			name:     "empty and relative dirs are skipped",
			runnerID: "probe",
			dirs:     []string{"", "  ", "relative/path", "/abs/ok"},
			want:     []string{"--no-tui", "--runner-id", "probe", "--dir", "/abs/ok", "--remote-control-terminal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ampLaunchArgs(tc.runnerID, tc.dirs, tc.discover)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("ampLaunchArgs = %v, want %v", got, tc.want)
			}
		})
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

// TestRunAmpLaunchesMultiDirFlags pins the host-runner shape: discover plus
// explicit --dir entries from the registry land in amp's argv in order.
func TestRunAmpLaunchesMultiDirFlags(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	registryFile := "" +
		"AGENTMUX_INSTANCE_NAME=probe\n" +
		"AGENTMUX_AGENT=amp\n" +
		"AGENTMUX_AMP_RUNNER_ID=probe\n" +
		"AGENTMUX_AMP_DIRS=/srv/hostel, /srv/extra\n" +
		"AGENTMUX_AMP_DISCOVER_DIRS=1\n" +
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
		"amp", "--no-tui", "--runner-id", "probe",
		"--discover-dirs", "--dir", "/srv/hostel", "--dir", "/srv/extra",
		"--remote-control-terminal",
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
		"No API key found. Starting login flow...\nTo log in, visit:\n\nhttps://auth.ampcode.com/device?user_code=AAAA-BBBB\n\nWaiting for confirmation in the browser...",
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

func TestAmpUpdateTargetVersion(t *testing.T) {
	cases := []struct {
		name, out, want string
	}{
		{
			// Captured verbatim shape from a live host: the version pin
			// precedes the failing pnpm invocation.
			name: "pinned version",
			out:  "Updating to version 0.0.1789603265-ge0868f...\nRunning: pnpm add -g @ampcode/cli@0.0.1789603265-ge0868f\n",
			want: "0.0.1789603265-ge0868f",
		},
		{
			name: "no version line",
			out:  "Error: pnpm add -g @ampcode/cli failed with code 1:\n ERR_PNPM_NO_GLOBAL_BIN_DIR  Unable to find the global bin directory\n",
			want: "",
		},
		{
			name: "empty",
			out:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ampUpdateTargetVersion(tc.out); got != tc.want {
				t.Errorf("ampUpdateTargetVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// ampScript is one scripted command outcome for fakeAmpRun.
type ampScript struct {
	out string
	err error
}

// fakeAmpRun scripts command outcomes by "name arg..." key and logs every
// invocation, so tests can assert npm was (or was not) attempted.
// "amp --version" is served from versions in call order (before-update
// probe first, after-update probe second) since one key maps to two
// different answers.
type fakeAmpRun struct {
	outputs  map[string]ampScript
	versions []ampScript
	calls    []string
}

func (f *fakeAmpRun) run(name string, args ...string) ([]byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, key)
	if key == "amp --version" && len(f.versions) > 0 {
		v := f.versions[0]
		f.versions = f.versions[1:]
		return []byte(v.out), v.err
	}
	if o, ok := f.outputs[key]; ok {
		return []byte(o.out), o.err
	}
	return nil, errors.New("unexpected command " + key)
}

func (f *fakeAmpRun) ran(prefix string) (string, bool) {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return c, true
		}
	}
	return "", false
}

func TestRunAmpUpdate(t *testing.T) {
	const pnpmFailPinned = "Updating to version 0.0.1789603265-ge0868f...\n" +
		"Running: pnpm add -g @ampcode/cli@0.0.1789603265-ge0868f\n" +
		"Error: pnpm add -g @ampcode/cli@0.0.1789603265-ge0868f failed with code 1:\n" +
		" ERR_PNPM_NO_GLOBAL_BIN_DIR  Unable to find the global bin directory\n"
	const pnpmFailUnpinned = "Error: pnpm add -g @ampcode/cli failed with code 1:\n" +
		" ERR_PNPM_NO_GLOBAL_BIN_DIR  Unable to find the global bin directory\n"
	const oldVersion = "0.0.1789329654-g2cdf19 (released 2026-09-13T20:00:54.000Z, 3d ago)\n"
	const newVersion = "0.0.1789603265-ge0868f (released 2026-09-17T00:01:05.000Z, 1h ago)\n"

	errUpdate := errors.New("exit status 1")
	errNpm := errors.New("exit status 1")
	errVersion := errors.New("exit status 1")

	type script = ampScript
	cases := []struct {
		name           string
		update         script
		versions       []script // before-update probe, then after-update probe
		npm            script
		npmSpec        string // expected "npm install -g ..." invocation; "" means npm must not run
		wantChanged    bool
		wantRecognized bool
		wantErr        bool
	}{
		{
			name:           "porcelain no change passes through",
			update:         script{"Checking for updates...\nno update needed\n", nil},
			wantChanged:    false,
			wantRecognized: true,
		},
		{
			name:           "porcelain updated passes through",
			update:         script{"Checking for updates...\nupdated 0.0.1789603265-ge0868f\n", nil},
			wantChanged:    true,
			wantRecognized: true,
		},
		{
			// A non-pnpm failure must propagate untouched: the npm route
			// would be wrong for e.g. a curl-installed amp, and attempting
			// it could paper over the real error.
			name:    "non-pnpm error propagates without npm",
			update:  script{"network unreachable\n", errUpdate},
			wantErr: true,
		},
		{
			name:           "pnpm failure falls back to pinned npm install",
			update:         script{pnpmFailPinned, errUpdate},
			versions:       []script{{oldVersion, nil}, {newVersion, nil}},
			npm:            script{"changed 2 packages in 6s\n", nil},
			npmSpec:        "npm install -g @ampcode/cli@0.0.1789603265-ge0868f",
			wantChanged:    true,
			wantRecognized: true,
		},
		{
			name:           "pnpm failure without version line installs latest",
			update:         script{pnpmFailUnpinned, errUpdate},
			versions:       []script{{oldVersion, nil}, {newVersion, nil}},
			npm:            script{"changed 2 packages in 6s\n", nil},
			npmSpec:        "npm install -g @ampcode/cli@latest",
			wantChanged:    true,
			wantRecognized: true,
		},
		{
			// npm "succeeding" without moving the version must not restart
			// the session: no version change, session already running.
			name:           "fallback with unchanged version reports no change",
			update:         script{pnpmFailPinned, errUpdate},
			versions:       []script{{oldVersion, nil}, {oldVersion, nil}},
			npm:            script{"up to date\n", nil},
			npmSpec:        "npm install -g @ampcode/cli@0.0.1789603265-ge0868f",
			wantChanged:    false,
			wantRecognized: true,
		},
		{
			name:     "npm fallback failure returns error",
			update:   script{pnpmFailPinned, errUpdate},
			versions: []script{{oldVersion, nil}},
			npm:      script{"npm error code EAI_AGAIN\n", errNpm},
			npmSpec:  "npm install -g @ampcode/cli@0.0.1789603265-ge0868f",
			wantErr:  true,
		},
		{
			name:     "unrunnable amp after fallback errors",
			update:   script{pnpmFailPinned, errUpdate},
			versions: []script{{oldVersion, nil}, {"", errVersion}},
			npm:      script{"changed 2 packages in 6s\n", nil},
			npmSpec:  "npm install -g @ampcode/cli@0.0.1789603265-ge0868f",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAmpRun{
				outputs: map[string]ampScript{
					"amp update --porcelain": tc.update,
				},
				versions: tc.versions,
			}
			if tc.npmSpec != "" {
				fake.outputs[tc.npmSpec] = tc.npm
			}
			out, changed, recognized, err := runAmpUpdate(fake.run)
			if (err != nil) != tc.wantErr {
				t.Fatalf("runAmpUpdate err = %v, wantErr %v (out %q)", err, tc.wantErr, out)
			}
			if tc.wantErr {
				if tc.npmSpec != "" {
					if got, ok := fake.ran("npm install -g "); !ok || got != tc.npmSpec {
						t.Errorf("runAmpUpdate ran npm %q, want %q before failing", got, tc.npmSpec)
					}
				} else if got, ok := fake.ran("npm "); ok {
					t.Errorf("runAmpUpdate ran npm (%q) on the non-fallback error path", got)
				}
				return
			}
			if changed != tc.wantChanged || recognized != tc.wantRecognized {
				t.Errorf("runAmpUpdate = (%v, %v), want (%v, %v)", changed, recognized, tc.wantChanged, tc.wantRecognized)
			}
			if tc.npmSpec == "" {
				if got, ok := fake.ran("npm "); ok {
					t.Errorf("runAmpUpdate ran npm (%q) on the non-fallback path", got)
				}
				return
			}
			if got, ok := fake.ran("npm install -g "); !ok || got != tc.npmSpec {
				t.Errorf("runAmpUpdate ran npm %q, want %q", got, tc.npmSpec)
			}
		})
	}
}
