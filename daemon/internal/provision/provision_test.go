package provision

import (
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

func TestIsCompactSummaryLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"compact summary", `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary"}}`, true},
		{"explicit false", `{"type":"user","isCompactSummary":false}`, false},
		{"field absent", `{"type":"assistant","message":{"role":"assistant"}}`, false},
		{"empty line", ``, false},
		{"malformed json", `{not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCompactSummaryLine([]byte(c.line)); got != c.want {
				t.Errorf("isCompactSummaryLine(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

func TestLastLine(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"multiple lines", "first\nsecond\nthird\n", "third"},
		{"no trailing newline", "first\nsecond\nthird", "third"},
		{"single line", "only\n", "only"},
		{"empty file", "", ""},
		{"large last line spans read window", strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 200*1024), strings.Repeat("b", 200*1024)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".jsonl")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := lastLine(path)
			if err != nil {
				t.Fatalf("lastLine: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("lastLine() = %d bytes, want %d bytes (mismatch)", len(got), len(c.want))
			}
		})
	}
}
