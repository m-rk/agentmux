package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
			runnerID: "webapp",
			want:     []string{"--no-tui", "--runner-id", "webapp", "--remote-control-terminal"},
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
// the runner's working directory, its tmux session/socket, the exact amp
// argv from ampcode.com/docs/cli/runners, and the host mode as -m
// (AMUX-36: no runner starts without it).
func TestRunAmpLaunchesTheDocumentedCommand(t *testing.T) {
	dir := withEnvDir(t)
	withTestHostMode(t, "high")
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
		"-m", "high",
	}
	if strings.Join(launch, " ") != strings.Join(want, " ") {
		t.Errorf("RunAmp launched\n  %v\nwant\n  %v", launch, want)
	}
}

// TestRunAmpRefusesWithoutMode is the AMUX-36 guard on the runner path:
// with no host mode and no instance override the runner refuses instead
// of starting on amp's default model.
func TestRunAmpRefusesWithoutMode(t *testing.T) {
	dir := withEnvDir(t)
	t.Setenv("HOME", t.TempDir()) // no amp.yaml anywhere
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
	calls := fakeTmux(t)
	if err := RunAmp("probe"); err == nil {
		t.Fatal("RunAmp started with no mode configured")
	} else if !strings.Contains(err.Error(), "no amp mode configured") {
		t.Fatalf("RunAmp error = %v, want the no-mode refusal", err)
	}
	for _, args := range *calls {
		if slices.Contains(args, "new-session") {
			t.Fatalf("a session was started despite the missing mode: %v", args)
		}
	}
}

// TestRunAmpLaunchesMultiDirFlags pins the host-runner shape: discover plus
// explicit --dir entries from the registry land in amp's argv in order,
// with the host mode as -m (AMUX-36).
func TestRunAmpLaunchesMultiDirFlags(t *testing.T) {
	dir := withEnvDir(t)
	withTestHostMode(t, "high")
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
		"-m", "high",
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

// TestTaskAmpStubArgsStampsTaskInstancesOnly pins the wrapper wiring: a
// task-* instance gets the -e PATH pair plus its effective mode, every
// other instance gets nothing, and a task instance with no mode anywhere
// gets nothing either (the runner itself refuses).
func TestTaskAmpStubArgsStampsTaskInstancesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostModeFile(t, home, "high")
	got := taskAmpStubArgs("task-9", map[string]string{})
	if len(got) != 4 || got[0] != "-e" || !strings.HasPrefix(got[1], "PATH=") || !strings.HasSuffix(got[1], ":$PATH") {
		t.Fatalf("taskAmpStubArgs(task-9) = %q, want the -e PATH pair", got)
	}
	if got[2] != "-e" || got[3] != "AGENTMUX_AMP_MODE=high" {
		t.Fatalf("taskAmpStubArgs(task-9) = %q, want the stamped host mode", got)
	}
	// The instance override wins over the host file.
	got = taskAmpStubArgs("task-9", map[string]string{"AGENTMUX_AMP_MODE": "custom"})
	if len(got) != 4 || got[3] != "AGENTMUX_AMP_MODE=custom" {
		t.Fatalf("taskAmpStubArgs(task-9, override) = %q, want the override", got)
	}
	if got := taskAmpStubArgs("site-amp", map[string]string{}); len(got) != 0 {
		t.Fatalf("taskAmpStubArgs(site-amp) = %q, want nothing", got)
	}
	// The wrapper script exists and is executable.
	if fi, err := os.Stat(filepath.Join(home, ".agentmux", "stubs", "amp")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("wrapper missing or not executable: %v", fi)
	}
}

// TestTaskAmpStubArgsRefusesWithoutMode: with no host mode and no
// instance override the wrapper pairs are absent — a silent run on amp's
// default model is never wired up.
func TestTaskAmpStubArgsRefusesWithoutMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no amp.yaml anywhere
	if got := taskAmpStubArgs("task-9", map[string]string{}); len(got) != 0 {
		t.Fatalf("taskAmpStubArgs(task-9) without a mode = %q, want nothing", got)
	}
}

// TestEnsureTaskAmpStubRewritesDrift upgrades the AMUX-36 refusing stub
// in place: an old stub file is rewritten to the wrapper.
func TestEnsureTaskAmpStubRewritesDrift(t *testing.T) {
	dir := t.TempDir()
	old := "#!/bin/sh\necho \"amp: agentmux starts amp for you; test with fakes or -dry-run\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "amp"), []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureTaskAmpStub(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "amp"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "exit 1\n") && !strings.Contains(string(data), "-m") {
		t.Fatalf("old refusing stub survived: %q", data)
	}
	// A second call is a no-op (identical content left alone).
	fi1, _ := os.Stat(filepath.Join(dir, "amp"))
	if err := ensureTaskAmpStub(dir); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(filepath.Join(dir, "amp"))
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("ensureTaskAmpStub rewrote an identical wrapper")
	}
}

// wrapperHarness installs the generated wrapper with a fake amp behind
// it: bindir holds an `amp` shell script recording its argv to argvFile,
// and the wrapper dir holds the real generated script. It returns the
// environment (PATH with the wrapper first, AGENTMUX_AMP_MODE set) for
// running probe commands through the wrapper.
func wrapperHarness(t *testing.T, mode string) (wrapper string, bindir, argvFile string, env []string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubDir := filepath.Join(home, ".agentmux", "stubs")
	if err := ensureTaskAmpStub(stubDir); err != nil {
		t.Fatal(err)
	}
	wrapper = filepath.Join(stubDir, "amp")
	bindir = t.TempDir()
	argvFile = filepath.Join(t.TempDir(), "argv")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argvFile + "\"\n"
	if err := os.WriteFile(filepath.Join(bindir, "amp"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	env = []string{
		"PATH=" + stubDir + ":" + bindir + ":/usr/bin:/bin",
		"AGENTMUX_AMP_MODE=" + mode,
		"HOME=" + home,
	}
	return wrapper, bindir, argvFile, env
}

func runWrapper(t *testing.T, wrapper string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(wrapper, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func readArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake amp never ran: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// TestTaskAmpWrapperAddsHostMode is the AMUX-45 back-test: a probe run
// through the wrapper the way a worker would run it reaches the real
// (here fake) amp with the host mode as -m.
func TestTaskAmpWrapperAddsHostMode(t *testing.T) {
	wrapper, _, argvFile, env := wrapperHarness(t, "high")
	env = append(env, AllowLiveAmpEnv+"=1")
	for _, args := range [][]string{
		{"-x", "reply with exactly: MODE-PROBE-OK and nothing else"},
		{"--execute", "probe"},
		{"threads", "new"},
		{"t", "c", "T-01a1119c-1111-4111-8111-111111111111"},
		{"threads", "continue", "T-01a1119c-1111-4111-8111-111111111111"},
		{"last"},
	} {
		os.Remove(argvFile)
		if _, err := runWrapper(t, wrapper, env, args...); err != nil {
			t.Fatalf("wrapper %v: %v", args, err)
		}
		got := readArgv(t, argvFile)
		if len(got) < 2 || got[0] != "-m" || got[1] != "high" {
			t.Fatalf("wrapper %v reached amp as %q, want -m high first", args, got)
		}
		if strings.Join(got[2:], " ") != strings.Join(args, " ") {
			t.Fatalf("wrapper %v reached amp as %q, want the args carried through", args, got)
		}
	}
}

// TestTaskAmpWrapperPassesReadsThrough: read-only calls reach amp
// without -m, exactly as typed.
func TestTaskAmpWrapperPassesReadsThrough(t *testing.T) {
	wrapper, _, argvFile, env := wrapperHarness(t, "high")
	for _, args := range [][]string{
		{"threads", "list"},
		{"threads", "export", "T-01a1119c-1111-4111-8111-111111111111"},
		{"version"},
		{"--version"},
	} {
		os.Remove(argvFile)
		if _, err := runWrapper(t, wrapper, env, args...); err != nil {
			t.Fatalf("wrapper %v: %v", args, err)
		}
		if got, want := readArgv(t, argvFile), args; strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("wrapper %v reached amp as %q, want passthrough", args, got)
		}
	}
}

// TestTaskAmpWrapperNeverDoublesMode: an explicit -m/--mode rides
// through untouched — the worker's (or agentmux's) choice wins.
func TestTaskAmpWrapperNeverDoublesMode(t *testing.T) {
	wrapper, _, argvFile, env := wrapperHarness(t, "high")
	env = append(env, AllowLiveAmpEnv+"=1")
	for _, args := range [][]string{
		{"-m", "custom", "-x", "probe"},
		{"--mode", "custom", "threads", "new"},
		{"--mode=custom", "-x", "probe"},
		{"-x", "--mode", "custom", "probe"},
	} {
		os.Remove(argvFile)
		if _, err := runWrapper(t, wrapper, env, args...); err != nil {
			t.Fatalf("wrapper %v: %v", args, err)
		}
		if got, want := readArgv(t, argvFile), args; strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("wrapper %v reached amp as %q, want no injected -m", args, got)
		}
	}
}

// TestTaskAmpWrapperRefusesWithoutMode: no mode, no thread — the
// wrapper exits non-zero before any amp runs.
func TestTaskAmpWrapperRefusesWithoutMode(t *testing.T) {
	wrapper, _, argvFile, env := wrapperHarness(t, "")
	envNoMode := []string{"PATH=" + strings.Split(env[0], "=")[1], AllowLiveAmpEnv + "=1"}
	out, err := runWrapper(t, wrapper, envNoMode, "-x", "probe")
	if err == nil {
		t.Fatal("wrapper ran with no mode configured")
	}
	if !strings.Contains(out, taskAmpStubRefusal) {
		t.Fatalf("wrapper refusal = %q, want the no-manual-runs text", out)
	}
	if _, serr := os.Stat(argvFile); !os.IsNotExist(serr) {
		t.Fatal("fake amp ran despite the missing mode")
	}
}

// TestTaskAmpWrapperFindsRealAmp: the wrapper never calls itself —
// argv shows the fake behind it ran, exactly once.
func TestTaskAmpWrapperFindsRealAmp(t *testing.T) {
	wrapper, _, argvFile, env := wrapperHarness(t, "high")
	if _, err := runWrapper(t, wrapper, env, "threads", "list"); err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	if got := readArgv(t, argvFile); strings.Join(got, " ") != "threads list" {
		t.Fatalf("fake amp saw %q", got)
	}
}

// TestRunAmpStampsStubPathOnTaskLaunch pins the full tmux argv for a
// task instance: the wrapper -e pairs land before the runner command,
// so a bare `amp` in the worker's own pane hits the wrapper with the
// host mode, and the stamped mode matches the runner's own -m.
func TestRunAmpStampsStubPathOnTaskLaunch(t *testing.T) {
	dir := withEnvDir(t)
	withTestHostMode(t, "high")
	workdir := t.TempDir()
	registryFile := "" +
		"AGENTMUX_INSTANCE_NAME=task-9\n" +
		"AGENTMUX_AGENT=amp\n" +
		"AGENTMUX_AMP_RUNNER_ID=task-9\n" +
		"AGENTMUX_TMUX_SESSION_NAME=task-9\n" +
		"AGENTMUX_WORKDIR=" + workdir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "task-9.env"), []byte(registryFile), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := fakeTmux(t)
	if err := RunAmp("task-9"); err != nil {
		t.Fatalf("RunAmp: %v", err)
	}
	got := newSessionArgs(t, *calls)
	flat := strings.Join(got, " ")
	if !strings.Contains(flat, "-e PATH=") {
		t.Fatalf("task launch carries no wrapper PATH: %v", got)
	}
	if !strings.Contains(flat, "AGENTMUX_AMP_MODE=high") {
		t.Fatalf("task launch carries no stamped mode: %v", got)
	}
	// The -e pairs precede the runner command.
	ei, ai := -1, -1
	for i, a := range got {
		if a == "-e" {
			ei = i
		}
		if a == "amp" {
			ai = i
		}
	}
	if ei < 0 || ai < 0 || ei > ai {
		t.Fatalf("wrapper -e must precede amp: %v", got)
	}
	// The stamped mode matches the runner's own -m.
	mi := -1
	for i, a := range got {
		if a == "-m" {
			mi = i
		}
	}
	if mi < 0 || mi+1 >= len(got) || got[mi+1] != "high" {
		t.Fatalf("runner carries no -m high: %v", got)
	}
}

var ampLiveCalls = [][]string{
	{"-x", "probe"},
	{"--execute", "probe"},
	{"threads", "continue", "-x", "go on", "T-01a1119c-1111-4111-8111-111111111111"},
	{"-m", "custom", "-x", "probe"},
}

// TestTaskAmpWrapperRefusesLiveExecute: -x never reaches amp without the
// opt-in, and the refusal is one line on stderr and one in the task log.
func TestTaskAmpWrapperRefusesLiveExecute(t *testing.T) {
	for _, args := range ampLiveCalls {
		wrapper, _, argvFile, env := wrapperHarness(t, "high")
		taskLog := filepath.Join(t.TempDir(), "task.log")
		env = append(env, "AGENTMUX_TASK_LOG="+taskLog)
		out, err := runWrapper(t, wrapper, env, args...)
		if err == nil {
			t.Fatalf("wrapper %v ran a live execute without the opt-in", args)
		}
		if !strings.Contains(out, taskAmpLiveRefusal) || strings.Count(strings.TrimSpace(out), "\n") != 0 {
			t.Fatalf("refusal = %q, want the one-line text", out)
		}
		if !strings.Contains(out, "fakeamp") {
			t.Fatalf("refusal = %q, want a pointer at the fake", out)
		}
		logged, _ := os.ReadFile(taskLog)
		if strings.TrimSpace(string(logged)) != "amp: "+taskAmpLiveRefusal {
			t.Fatalf("task log = %q, want exactly the refusal line", logged)
		}
		if _, serr := os.Stat(argvFile); !os.IsNotExist(serr) {
			t.Fatalf("amp ran despite the refusal (%v)", args)
		}
	}
}

// TestTaskAmpWrapperAllowsLiveWithFlag: with the opt-in the call reaches
// amp (the fake here) and nothing is logged.
func TestTaskAmpWrapperAllowsLiveWithFlag(t *testing.T) {
	for _, args := range ampLiveCalls {
		wrapper, _, argvFile, env := wrapperHarness(t, "high")
		taskLog := filepath.Join(t.TempDir(), "task.log")
		env = append(env, AllowLiveAmpEnv+"=1", "AGENTMUX_TASK_LOG="+taskLog)
		if out, err := runWrapper(t, wrapper, env, args...); err != nil {
			t.Fatalf("wrapper %v with the flag: %v: %s", args, err, out)
		}
		if got := readArgv(t, argvFile); len(got) == 0 {
			t.Fatalf("amp did not run for %v", args)
		}
		if _, serr := os.Stat(taskLog); !os.IsNotExist(serr) {
			t.Fatalf("task log written for an allowed call (%v)", args)
		}
	}
}

// TestTaskAmpWrapperLiveGuardLeavesRunnerAlone: the guard touches only -x.
// The runner's own launch args (--no-tui, no -x) and non-executing calls
// reach amp as before with no flag set and nothing logged.
func TestTaskAmpWrapperLiveGuardLeavesRunnerAlone(t *testing.T) {
	for _, args := range [][]string{
		{"--no-tui", "--runner-id", "r1", "--remote-control-terminal", "-m", "high"},
		{"threads", "list"},
		{"threads", "continue", "T-01a1119c-1111-4111-8111-111111111111"},
		{"--version"},
	} {
		wrapper, _, argvFile, env := wrapperHarness(t, "high")
		taskLog := filepath.Join(t.TempDir(), "task.log")
		env = append(env, "AGENTMUX_TASK_LOG="+taskLog)
		if out, err := runWrapper(t, wrapper, env, args...); err != nil {
			t.Fatalf("wrapper %v: %v: %s", args, err, out)
		}
		if got := readArgv(t, argvFile); len(got) == 0 {
			t.Fatalf("amp did not run for %v", args)
		}
		if _, serr := os.Stat(taskLog); !os.IsNotExist(serr) {
			t.Fatalf("task log written for %v", args)
		}
	}
}
