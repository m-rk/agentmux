package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// withEnvDir points discovery.EnvDir at a fresh temp dir for the duration
// of the test, restoring the previous value afterward — the same override
// mechanism `agentmux daemon run -env-dir` uses in production, here reused
// so tests never touch a real /etc/agentmux or ~/.agentmux/registry.
func withEnvDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = prev })
	return dir
}

// withUnitFileExists stubs the platform's unitFileExists (a var
// specifically so tests can do this) so guardAgentMismatch tests don't
// depend on the real ~/Library/LaunchAgents or /etc/systemd/system.
func withUnitFileExists(t *testing.T, names map[string]bool) {
	t.Helper()
	prev := unitFileExists
	unitFileExists = func(name string) bool { return names[name] }
	t.Cleanup(func() { unitFileExists = prev })
}

func TestValidateIdentifier(t *testing.T) {
	valid := []string{"claude-code", "probe_2", "a.b-c", "ABC123"}
	for _, v := range valid {
		if err := validateIdentifier("test", v); err != nil {
			t.Errorf("validateIdentifier(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{"", "has space", "has/slash", "has$dollar", "emoji🤹"}
	for _, v := range invalid {
		if err := validateIdentifier("test", v); err == nil {
			t.Errorf("validateIdentifier(%q) = nil, want an error", v)
		}
	}
}

func TestExistingAgentFor(t *testing.T) {
	dir := withEnvDir(t)

	t.Run("no file at all", func(t *testing.T) {
		_, exists := existingAgentFor("nope")
		if exists {
			t.Error("exists = true, want false for a name with no registry file")
		}
	})

	t.Run("file with explicit AGENTMUX_AGENT", func(t *testing.T) {
		write(t, dir, "probe-zero.env", "AGENTMUX_INSTANCE_NAME=probe-zero\nAGENTMUX_AGENT=zero\n")
		agent, exists := existingAgentFor("probe-zero")
		if !exists || agent != "zero" {
			t.Errorf("existingAgentFor = (%q, %v), want (\"zero\", true)", agent, exists)
		}
	})

	t.Run("file present but AGENTMUX_AGENT absent defaults to claude-code", func(t *testing.T) {
		// The claude-code provisioners never write AGENTMUX_AGENT (it
		// predates zero/opencode); discovery.go's own default matches this.
		write(t, dir, "claude-code.env", "AGENTMUX_INSTANCE_NAME=claude-code\nAGENTMUX_WORKDIR=/w\n")
		agent, exists := existingAgentFor("claude-code")
		if !exists || agent != "claude-code" {
			t.Errorf("existingAgentFor = (%q, %v), want (\"claude-code\", true)", agent, exists)
		}
	})
}

func TestGuardAgentMismatch(t *testing.T) {
	dir := withEnvDir(t)

	t.Run("no existing instance, no unit file: allowed", func(t *testing.T) {
		withUnitFileExists(t, nil)
		if err := guardAgentMismatch("brand-new", "zero"); err != nil {
			t.Errorf("guardAgentMismatch = %v, want nil", err)
		}
	})

	t.Run("registered instance, same agent: allowed (re-provisioning)", func(t *testing.T) {
		withUnitFileExists(t, nil)
		write(t, dir, "probe-zero.env", "AGENTMUX_AGENT=zero\n")
		if err := guardAgentMismatch("probe-zero", "zero"); err != nil {
			t.Errorf("guardAgentMismatch = %v, want nil", err)
		}
	})

	t.Run("registered instance, different agent: refused", func(t *testing.T) {
		withUnitFileExists(t, nil)
		write(t, dir, "probe-zero.env", "AGENTMUX_AGENT=zero\n")
		err := guardAgentMismatch("probe-zero", "opencode")
		if err == nil {
			t.Fatal("guardAgentMismatch = nil, want an error for a cross-agent conflict")
		}
	})

	t.Run("no registry entry but a unit file already exists (pre-registry bash install): refused", func(t *testing.T) {
		// This is the scenario that actually matters: an instance installed
		// by backends/claude-code/install-macos.sh (or install.sh) has no
		// *.env file at all, so existingAgentFor alone would wrongly approve
		// overwriting it.
		withUnitFileExists(t, map[string]bool{"claude-code": true})
		err := guardAgentMismatch("claude-code", "zero")
		if err == nil {
			t.Fatal("guardAgentMismatch = nil, want an error for a pre-registry unit collision")
		}
	})

	t.Run("no registry entry and no unit file: allowed", func(t *testing.T) {
		withUnitFileExists(t, map[string]bool{"some-other-name": true})
		if err := guardAgentMismatch("totally-fresh", "opencode"); err != nil {
			t.Errorf("guardAgentMismatch = %v, want nil", err)
		}
	})
}

func TestDisplayNameFor(t *testing.T) {
	prevRealUserCount := realUserCount
	t.Cleanup(func() { realUserCount = prevRealUserCount })

	t.Run("single real user: no user prefix", func(t *testing.T) {
		realUserCount = func() int { return 1 }
		got := DisplayNameFor("testuser", "/home/testuser/.agentmux/probe")
		if !strings.HasSuffix(got, "🤹 probe") {
			t.Errorf("DisplayNameFor = %q, want suffix %q", got, "🤹 probe")
		}
		if strings.HasPrefix(got, "testuser:") {
			t.Errorf("DisplayNameFor = %q, should not have a user prefix on a single-user machine", got)
		}
	})

	t.Run("multiple real users: user prefix included", func(t *testing.T) {
		realUserCount = func() int { return 2 }
		got := DisplayNameFor("testuser", "/home/testuser/.agentmux/probe")
		if !strings.HasPrefix(got, "testuser:") {
			t.Errorf("DisplayNameFor = %q, want prefix %q", got, "testuser:")
		}
	})

	t.Run("explicit host name overrides the derived host", func(t *testing.T) {
		realUserCount = func() int { return 1 }
		got := DisplayNameForHost("testuser", "build-box", "/home/testuser/.agentmux/probe")
		if want := "build-box 🤹 probe"; got != want {
			t.Errorf("DisplayNameForHost = %q, want %q", got, want)
		}
	})
}

func TestResolveHostName(t *testing.T) {
	got, err := resolveHostName("  build-box  ")
	if err != nil {
		t.Fatalf("resolveHostName: %v", err)
	}
	if got != "build-box" {
		t.Fatalf("resolveHostName = %q, want build-box", got)
	}
	if _, err := resolveHostName("not a host"); err == nil {
		t.Error("resolveHostName accepted spaces")
	}
}

func TestProviderBaseURL(t *testing.T) {
	if got, want := providerBaseURL("ollama"), "http://localhost:11434/v1"; got != want {
		t.Errorf("providerBaseURL(ollama) = %q, want %q", got, want)
	}
	if got := providerBaseURL("something-else"); got != "" {
		t.Errorf("providerBaseURL(something-else) = %q, want empty", got)
	}
}

func TestValidateSupportedAgentProvider(t *testing.T) {
	// Any provider id is accepted for zero/opencode/kilo — it's no longer
	// an ollama-only allowlist, so a custom OpenAI-compatible gateway (e.g.
	// "my-company-gateway") is just as valid as "ollama" here. Only the
	// agent side is actually checked; resolveBaseURL is what enforces a
	// non-ollama provider supplies its own base URL.
	valid := [][2]string{
		{"zero", "ollama"}, {"opencode", "ollama"}, {"kilo", "ollama"},
		{"zero", "custom-gateway"}, {"opencode", "custom-gateway"}, {"kilo", "custom-gateway"},
	}
	for _, v := range valid {
		if err := validateSupportedAgentProvider(v[0], v[1]); err != nil {
			t.Errorf("validateSupportedAgentProvider(%q, %q) = %v, want nil", v[0], v[1], err)
		}
	}

	invalid := [][2]string{{"claude-code", "ollama"}, {"unknown", "unknown"}}
	for _, v := range invalid {
		if err := validateSupportedAgentProvider(v[0], v[1]); err == nil {
			t.Errorf("validateSupportedAgentProvider(%q, %q) = nil, want an error", v[0], v[1])
		}
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Run("ollama with no explicit URL: built-in default", func(t *testing.T) {
		got, err := resolveBaseURL("ollama", "")
		if err != nil {
			t.Fatalf("resolveBaseURL: %v", err)
		}
		if want := "http://localhost:11434/v1"; got != want {
			t.Errorf("resolveBaseURL(ollama, \"\") = %q, want %q", got, want)
		}
	})

	t.Run("ollama with an explicit override: the override wins", func(t *testing.T) {
		got, err := resolveBaseURL("ollama", "http://elsewhere:11434/v1")
		if err != nil {
			t.Fatalf("resolveBaseURL: %v", err)
		}
		if want := "http://elsewhere:11434/v1"; got != want {
			t.Errorf("resolveBaseURL(ollama, override) = %q, want %q", got, want)
		}
	})

	t.Run("custom provider with an explicit URL: used as-is", func(t *testing.T) {
		got, err := resolveBaseURL("custom-gateway", "https://gateway.example/v1")
		if err != nil {
			t.Fatalf("resolveBaseURL: %v", err)
		}
		if want := "https://gateway.example/v1"; got != want {
			t.Errorf("resolveBaseURL(custom, url) = %q, want %q", got, want)
		}
	})

	t.Run("custom provider with no explicit URL: error", func(t *testing.T) {
		if _, err := resolveBaseURL("custom-gateway", ""); err == nil {
			t.Error("resolveBaseURL(custom, \"\") = nil error, want one (no default to fall back to)")
		}
	})
}

func TestKiloCustomProviderNote(t *testing.T) {
	got := kiloCustomProviderNote("acme", "https://acme.example/v1", "big-model", "ACME_API_KEY")
	for _, want := range []string{"acme", "https://acme.example/v1", "big-model", "ACME_API_KEY", "~/.config/kilo/kilo.jsonc", "~/.config/agentmux/kilo-env", "kilo roll-call big-model"} {
		if !strings.Contains(got, want) {
			t.Errorf("kiloCustomProviderNote output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteRegistryRoundTrip(t *testing.T) {
	dir := withEnvDir(t)

	path, err := writeRegistry("probe", []kv{
		{"AGENTMUX_INSTANCE_NAME", "probe"},
		{"AGENTMUX_AGENT", "zero"},
	})
	if err != nil {
		t.Fatalf("writeRegistry: %v", err)
	}
	if want := filepath.Join(dir, "probe.env"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	if want := "AGENTMUX_INSTANCE_NAME=probe\nAGENTMUX_AGENT=zero\n"; string(data) != want {
		t.Errorf("registry content = %q, want %q", string(data), want)
	}
}

func TestWriteRegistryRejectsInjectedFields(t *testing.T) {
	withEnvDir(t)

	tests := []struct {
		name   string
		fields []kv
	}{
		{"newline in value", []kv{{"AGENTMUX_WORKDIR", "/tmp/safe\nAGENTMUX_SERVICE_NAME=ssh.service"}}},
		{"carriage return in value", []kv{{"AGENTMUX_MODEL", "safe\rAGENTMUX_AGENT=claude-code"}}},
		{"separator in key", []kv{{"AGENTMUX_MODEL=unsafe", "value"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := writeRegistry("probe", tt.fields); err == nil {
				t.Fatal("expected unsafe registry field to be rejected")
			}
		})
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAtCompactBoundary(t *testing.T) {
	const (
		compactSummary = `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary"}}`
		bookkeeping    = `{"type":"attachment"}`
		continuePrompt = `{"type":"user","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"Continue from where you left off."}]}}`
		syntheticReply = `{"type":"assistant","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`
		realUserTurn   = `{"type":"user","message":{"role":"user","content":"do something"}}`
		realAssistant  = `{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"done"}]}}`
		// The /compact command's own echo of itself, appended after the
		// compact-summary entry it produced: a "<local-command-caveat>"
		// isMeta turn, then non-meta "<command-name>" and
		// "<local-command-stdout>" turns.
		compactCaveat      = `{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat: the messages below were generated by the user while running local commands.</local-command-caveat>"}}`
		compactCommandName = `{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>"}}`
		compactStdout      = `{"type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}`
	)

	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"empty transcript", nil, false},
		{"last line is the compact summary itself", []string{realAssistant, compactSummary}, true},
		{"real conversation after compact summary", []string{compactSummary, realUserTurn, realAssistant}, false},
		{
			"synthetic resume exchange right after a compact summary is still a boundary",
			[]string{realUserTurn, compactSummary, bookkeeping, continuePrompt, syntheticReply},
			true,
		},
		{
			"bookkeeping interspersed throughout doesn't change the answer",
			[]string{compactSummary, bookkeeping, bookkeeping, continuePrompt, bookkeeping, syntheticReply, bookkeeping},
			true,
		},
		{
			"synthetic reply with real content underneath is not a boundary",
			[]string{realUserTurn, realAssistant, continuePrompt, syntheticReply},
			false,
		},
		{"malformed json is skipped, not fatal", []string{compactSummary, `{not json`}, true},
		{
			// Regression for a real production bug: the old fixed
			// "newest 3 conversation turns" window saw through the
			// synthetic resume exchange but not also through the
			// /compact command's own echo of itself sitting between the
			// exchange and the actual compact-summary entry, so every
			// instance recompacted every single night regardless of
			// activity.
			"a full nightly cycle (compact echo, then next day's resume exchange) is still a boundary",
			[]string{realUserTurn, compactSummary, compactCaveat, compactCommandName, compactStdout, continuePrompt, syntheticReply},
			true,
		},
		{
			"compact echo lines are bookkeeping even without isMeta",
			[]string{compactSummary, compactCommandName, compactStdout},
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var lines [][]byte
			for _, l := range c.lines {
				lines = append(lines, []byte(l))
			}
			if got := atCompactBoundary(lines); got != c.want {
				t.Errorf("atCompactBoundary() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTailLines(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name     string
		content  string
		minLines int
		want     []string
	}{
		// A file smaller than the 64KB starting window is always read whole
		// in one pass, so minLines doesn't trim the result down to exactly
		// that many lines — it only guarantees "at least" when the window
		// has to grow.
		{"multiple lines, whole file fits in one window", "first\nsecond\nthird\n", 2, []string{"first", "second", "third"}},
		{"no trailing newline", "first\nsecond\nthird", 3, []string{"first", "second", "third"}},
		{"single line", "only\n", 1, []string{"only"}},
		{"empty file", "", 1, nil},
		{"fewer lines than requested returns what's there", "first\nsecond\n", 10, []string{"first", "second"}},
		{
			"large last line spans read window: grows past it to the real boundary rather than truncating",
			strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 200*1024),
			1,
			[]string{strings.Repeat("a", 100), strings.Repeat("b", 200*1024)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".jsonl")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := tailLines(path, c.minLines)
			if err != nil {
				t.Fatalf("tailLines: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("tailLines() = %d lines, want %d", len(got), len(c.want))
			}
			for i, l := range got {
				if string(l) != c.want[i] {
					t.Errorf("tailLines()[%d] = %d bytes, want %d bytes (mismatch)", i, len(l), len(c.want[i]))
				}
			}
		})
	}
}

// TestTailLinesGrowsAcrossWindows uses many mid-sized lines (padded well
// past the plain per-line overhead) so that the initial 64KB window falls
// short of minLines and tailLines has to double at least once — checking
// that growth doesn't introduce a truncated fragment at the start of the
// result once it does.
func TestTailLinesGrowsAcrossWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "growth.jsonl")

	const numLines = 20
	want := make([]string, numLines)
	var content strings.Builder
	for i := range numLines {
		want[i] = fmt.Sprintf("line-%03d-", i) + strings.Repeat("x", 5000)
		content.WriteString(want[i])
		content.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := tailLines(path, numLines-2) // short of the full file, forcing at least one doubling
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(got) < numLines-2 {
		t.Fatalf("tailLines() = %d lines, want at least %d", len(got), numLines-2)
	}
	// Every returned line must exactly match its counterpart from the end
	// of want — a truncated fragment from a mishandled window boundary
	// would show up here as a mismatch on the first returned line.
	offset := numLines - len(got)
	for i, l := range got {
		if string(l) != want[offset+i] {
			t.Errorf("tailLines()[%d] = %q, want %q", i, string(l), want[offset+i])
		}
	}
}
