package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanTelemetryEnvAccepts(t *testing.T) {
	got, err := CleanTelemetryEnv([]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer abc, x=y",
		"OTEL_SERVICE_NAME=",
	})
	if err != nil || len(got) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got[1].key != "AGENTMUX_ENV_OTEL_EXPORTER_OTLP_HEADERS" || got[1].value != "Authorization=Bearer abc, x=y" {
		t.Errorf("kv = %+v", got[1])
	}
}

func TestCleanTelemetryEnvRefusals(t *testing.T) {
	const secret = "hunter2value"
	for _, tc := range []struct{ in, wantKey string }{
		{"PATH=" + secret, "PATH"},
		{"OTEL_lower=" + secret, "OTEL_lower"},
		{"OTEL_=" + secret, "OTEL_"},
		{"CLAUDE_CODE_ENABLE_TELEMETRY_X=" + secret, "CLAUDE_CODE_ENABLE_TELEMETRY_X"},
		{"OTEL_SERVICE_NAME", "OTEL_SERVICE_NAME"},
		{"OTEL_SERVICE_NAME=a\nb" + secret, "OTEL_SERVICE_NAME"},
		{"OTEL_SERVICE_NAME=a\x00" + secret, "OTEL_SERVICE_NAME"},
		{"OTEL_SERVICE_NAME=a\r" + secret, "OTEL_SERVICE_NAME"},
		{"=" + secret, ""},
	} {
		_, err := CleanTelemetryEnv([]string{tc.in})
		if err == nil {
			t.Errorf("%q: want refusal", tc.in)
			continue
		}
		if tc.wantKey != "" && !strings.Contains(err.Error(), tc.wantKey) {
			t.Errorf("%q: error %q does not name the key", tc.in, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%q: error leaks the value: %v", tc.in, err)
		}
	}
	if _, err := CleanTelemetryEnv([]string{"OTEL_A=1", "OTEL_A=2"}); err == nil || !strings.Contains(err.Error(), "OTEL_A") {
		t.Errorf("duplicate: %v", err)
	}
}

func TestEnvRegistryRoundTrip(t *testing.T) {
	dir := withEnvDir(t)
	fields, err := CleanTelemetryEnv([]string{"OTEL_B=2", "OTEL_A=a=b,c=d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeRegistry("inst", append([]kv{{"AGENTMUX_INSTANCE_NAME", "inst"}}, fields...)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "inst.env"))
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, _ := strings.Cut(l, "=")
		m[k] = v
	}
	got := EnvFromRegistry(m)
	if len(got) != 2 || got[0] != "OTEL_A=a=b,c=d" || got[1] != "OTEL_B=2" {
		t.Fatalf("round trip = %q", got)
	}
	if !RegistryEnvMatches(m, []string{"OTEL_B=2", "OTEL_A=a=b,c=d"}) || RegistryEnvMatches(m, []string{"OTEL_B=3", "OTEL_A=a=b,c=d"}) {
		t.Error("RegistryEnvMatches wrong")
	}
}

func TestCreateEnvRefusedForOtherAgentsAndBadKeys(t *testing.T) {
	withEnvDir(t)
	_, err := Create(Options{Agent: "opencode", InstanceName: "x", Env: []string{"OTEL_A=1"}})
	if err == nil || !strings.Contains(err.Error(), "opencode") {
		t.Errorf("other agent: %v", err)
	}
	_, err = Create(Options{Agent: "claude-code", InstanceName: "x", Env: []string{"HOME=/secretpath"}})
	if err == nil || !strings.Contains(err.Error(), "HOME") || strings.Contains(err.Error(), "/secretpath") {
		t.Errorf("bad key: %v", err)
	}
}
