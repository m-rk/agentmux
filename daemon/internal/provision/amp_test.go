package provision

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestAmpRunnerID(t *testing.T) {
	cases := []struct {
		instance string
		want     string
	}{
		// The common case: an instance name that's already a valid label.
		{"kartography", "kartography"},
		{"data-import", "data-import"},
		// validateIdentifier accepts these, a DNS label does not.
		{"probe_2", "probe-2"},
		{"a.b-c", "a-b-c"},
		{"my_project.v2", "my-project-v2"},
		// amp treats runner IDs case-insensitively; folding keeps it stable.
		{"ABC123", "abc123"},
		{"Obsidian_LiveSync", "obsidian-livesync"},
		// A label may not start or end with a hyphen, and runs collapse.
		{"-lead", "lead"},
		{"trail-", "trail"},
		{"a__b", "a-b"},
		{"..x..", "x"},
		{"  spaced  ", "spaced"},
	}
	for _, tc := range cases {
		got, err := AmpRunnerID(tc.instance)
		if err != nil {
			t.Errorf("AmpRunnerID(%q) = error %v, want %q", tc.instance, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("AmpRunnerID(%q) = %q, want %q", tc.instance, got, tc.want)
		}
	}
}

// TestAmpRunnerIDDerivation documents the createAmp derivation: the runner
// id is the instance name with any trailing -<agent> suffix stripped, so
// the runner shown on ampcode.com is the clean project name rather than
// the redundant agentmux-suffixed instance name. The sanitizer itself
// (AmpRunnerID) is tested above; this asserts the *trim* step the
// provisioner applies before calling it.
func TestAmpRunnerIDDerivation(t *testing.T) {
	cases := []struct {
		name, agent, instance, want string
	}{
		// The default agentmux naming: instance is <workdir>-<agent>; the
		// redundant agent suffix is dropped for the runner id.
		{"34-surada-amp", "amp", "34-surada-amp", "34-surada"},
		{"34-surada-kilo", "kilo", "34-surada-kilo", "34-surada"},
		// An instance the operator named without the agent suffix: nothing
		// to strip; the runner id is the instance name itself.
		{"34-surada", "amp", "34-surada", "34-surada"},
		// A hand-picked instance name that doesn't end in -<agent>: also
		// left as-is.
		{"myproj", "amp", "myproj", "myproj"},
		// A hand-picked name that *does* end in -amp is treated as the
		// agentmux-appended suffix and trimmed; documented trade-off.
		{"myproj-amp", "amp", "myproj-amp", "myproj"},
		// But a name that merely *contains* -amp is left alone.
		{"my-amp-project", "amp", "my-amp-project", "my-amp-project"},
		// A different agent's suffix is NOT stripped — the trim only fires
		// when the suffix matches the current agent.
		{"34-surada-kilo", "amp", "34-surada-kilo", "34-surada-kilo"},
	}
	for _, tc := range cases {
		got, err := AmpRunnerID(strings.TrimSuffix(tc.instance, "-"+tc.agent))
		if err != nil {
			t.Errorf("derive(name=%q agent=%q) = error %v, want %q", tc.instance, tc.agent, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("derive(name=%q agent=%q) = %q, want %q", tc.instance, tc.agent, got, tc.want)
		}
	}
}
// TestAmpRunnerIDIsIdempotent is what lets ampRunnerIDFor push a registry
// value it didn't compute through the same function without having to tell
// "already sanitized" apart from "needs sanitizing".
func TestAmpRunnerIDIsIdempotent(t *testing.T) {
	for _, instance := range []string{"kartography", "probe_2", "A.B_C", "  --weird--  ", "x"} {
		once, err := AmpRunnerID(instance)
		if err != nil {
			t.Fatalf("AmpRunnerID(%q): %v", instance, err)
		}
		twice, err := AmpRunnerID(once)
		if err != nil {
			t.Fatalf("AmpRunnerID(%q) (second pass): %v", once, err)
		}
		if once != twice {
			t.Errorf("AmpRunnerID is not idempotent for %q: %q then %q", instance, once, twice)
		}
	}
}

func TestAmpRunnerIDTruncatesToALabel(t *testing.T) {
	long := strings.Repeat("a", 80)
	got, err := AmpRunnerID(long)
	if err != nil {
		t.Fatalf("AmpRunnerID(long): %v", err)
	}
	if len(got) != maxAmpRunnerIDLen {
		t.Errorf("len(AmpRunnerID(80 chars)) = %d, want %d", len(got), maxAmpRunnerIDLen)
	}

	// Truncation must not be allowed to leave a trailing hyphen behind,
	// which would make the result an invalid hostname label.
	trailing := strings.Repeat("a", maxAmpRunnerIDLen) + "-" + strings.Repeat("b", 10)
	got, err = AmpRunnerID(trailing)
	if err != nil {
		t.Fatalf("AmpRunnerID(trailing): %v", err)
	}
	if strings.HasSuffix(got, "-") || strings.HasPrefix(got, "-") {
		t.Errorf("AmpRunnerID(%q) = %q, want no leading/trailing hyphen", trailing, got)
	}
}

func TestAmpRunnerIDRejectsUnusableNames(t *testing.T) {
	for _, instance := range []string{"", "...", "___", "-", "   "} {
		if got, err := AmpRunnerID(instance); err == nil {
			t.Errorf("AmpRunnerID(%q) = %q, want an error (nothing usable survives sanitizing)", instance, got)
		}
	}
}

func TestRejectUnsupportedAmpOptions(t *testing.T) {
	if err := rejectUnsupportedAmpOptions(Options{
		InstanceName: "probe",
		Agent:        "amp",
		Workdir:      "/w",
		RunUser:      "dev",
		HostName:     "laptop",
	}); err != nil {
		t.Errorf("rejectUnsupportedAmpOptions on a plain amp request = %v, want nil", err)
	}

	cases := map[string]Options{
		"provider":             {Provider: "ollama"},
		"model":                {Model: "gpt-oss:20b"},
		"provider base URL":    {BaseURL: "https://gateway.example/v1"},
		"provider API key env": {APIKeyEnv: "GATEWAY_API_KEY"},
		"resume":               {ResumeSessionID: "abc123"},
		"compact":              {CompactOnUpdate: "off"},
	}
	for name, opts := range cases {
		opts.Agent = "amp"
		if err := rejectUnsupportedAmpOptions(opts); err == nil {
			t.Errorf("rejectUnsupportedAmpOptions accepted a %s for the amp agent, want an error", name)
		}
	}
}

func TestDefaultInstanceNameForAmp(t *testing.T) {
	if got, want := defaultInstanceName("amp", ""), "amp"; got != want {
		t.Errorf("defaultInstanceName(amp, \"\") = %q, want %q", got, want)
	}
	if got, want := defaultInstanceName("amp", "/home/dev/kartography"), "kartography-amp"; got != want {
		t.Errorf("defaultInstanceName(amp, workdir) = %q, want %q", got, want)
	}
}

// TestCreateRejectsUnknownAgentMentionsAmp guards the user-facing list of
// supported agents staying in sync with Create's own dispatch.
func TestCreateRejectsUnknownAgentMentionsAmp(t *testing.T) {
	withEnvDir(t)
	withUnitFileExists(t, nil)
	_, err := Create(Options{InstanceName: "probe", Agent: "nope"})
	if err == nil {
		t.Fatal("Create with an unknown agent = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "amp") {
		t.Errorf("Create's unsupported-agent error = %q, want it to list amp", err)
	}
}

// ampAuthProbe builds an *exec.Cmd that replays a canned exit status and
// output, standing in for the real `amp usage` call each platform wires up.
func ampAuthProbe(t *testing.T, output string, exitCode int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAmpUsageHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_AMP_USAGE_HELPER=1",
		"GO_AMP_USAGE_OUTPUT="+output,
		"GO_AMP_USAGE_EXIT="+strconv.Itoa(exitCode),
	)
	return cmd
}

func TestAmpAuthProblemVia(t *testing.T) {
	// Logged in: `amp usage` exits 0 and prints the credit balance.
	if problem := ampAuthProblemVia(ampAuthProbe(t, "Usage: 12 of 100 credits", 0)); problem != "" {
		t.Errorf("ampAuthProblemVia for a logged-in amp = %q, want \"\"", problem)
	}

	// Not logged in: the exact message amp 0.0.1789300838-gde32db prints.
	problem := ampAuthProblemVia(ampAuthProbe(t, "Error: Invalid or missing API key. Run 'amp login' to authenticate.", 1))
	if problem == "" {
		t.Fatal("ampAuthProblemVia for a logged-out amp = \"\", want a problem")
	}
	if !strings.Contains(problem, "not appear to be logged in") {
		t.Errorf("ampAuthProblemVia for a logged-out amp = %q, want it to say amp isn't logged in", problem)
	}

	// Anything else must not be misreported as a missing login, or the
	// operator gets sent chasing an `amp login` that isn't needed.
	problem = ampAuthProblemVia(ampAuthProbe(t, "Error: connect ETIMEDOUT 1.2.3.4:443", 1))
	if !strings.Contains(problem, "could not confirm") {
		t.Errorf("ampAuthProblemVia for an unrelated failure = %q, want a \"could not confirm\" report", problem)
	}
	if strings.Contains(problem, "\n") {
		t.Errorf("ampAuthProblemVia result spans multiple lines: %q", problem)
	}
}

func TestFirstLine(t *testing.T) {
	if got, want := firstLine("  one\ntwo\n", 100), "one"; got != want {
		t.Errorf("firstLine = %q, want %q", got, want)
	}
	if got := firstLine(strings.Repeat("x", 500), 10); got != strings.Repeat("x", 10)+"..." {
		t.Errorf("firstLine did not cap a long line: %q", got)
	}
}

func TestAmpUsageHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_AMP_USAGE_HELPER") != "1" {
		return
	}
	os.Stdout.WriteString(os.Getenv("GO_AMP_USAGE_OUTPUT"))
	if os.Getenv("GO_AMP_USAGE_EXIT") != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}

// npmLsProbe builds an *exec.Cmd that replays a canned exit status,
// standing in for the real `npm ls -g @sourcegraph/amp --depth=0` call each
// platform wires up.
func npmLsProbe(t *testing.T, exitCode int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNpmLsHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_NPM_LS_HELPER=1",
		"GO_NPM_LS_EXIT="+strconv.Itoa(exitCode),
	)
	return cmd
}

// TestAmpInstallPackageProblemVia guards against the mproject2000 incident:
// amp provisioned via `npm install -g @sourcegraph/amp` (the seemingly
// obvious package name) instead of @ampcode/cli, the package amp's own
// self-updater actually manages, so every `amp update` EEXIST'd on the amp
// bin symlink forever — not a race, confirmed to reproduce run after run
// even one at a time. See ampInstallPackageProblemVia's doc comment.
func TestAmpInstallPackageProblemVia(t *testing.T) {
	// @sourcegraph/amp present: `npm ls -g @sourcegraph/amp --depth=0`
	// exits 0 — this is the broken install.
	problem := ampInstallPackageProblemVia(npmLsProbe(t, 0))
	if problem == "" {
		t.Fatal("ampInstallPackageProblemVia with @sourcegraph/amp installed = \"\", want a problem")
	}
	if !strings.Contains(problem, "@sourcegraph/amp") || !strings.Contains(problem, "@ampcode/cli") {
		t.Errorf("ampInstallPackageProblemVia = %q, want it to name both packages and the fix", problem)
	}

	// @sourcegraph/amp absent: `npm ls -g @sourcegraph/amp --depth=0` exits
	// non-zero — this is the healthy install (amp installed via
	// @ampcode/cli directly, or not installed at all, which
	// checkAgentInstalled already gates separately).
	if problem := ampInstallPackageProblemVia(npmLsProbe(t, 1)); problem != "" {
		t.Errorf("ampInstallPackageProblemVia with @sourcegraph/amp absent = %q, want \"\"", problem)
	}
}

func TestNpmLsHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_NPM_LS_HELPER") != "1" {
		return
	}
	if os.Getenv("GO_NPM_LS_EXIT") != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
