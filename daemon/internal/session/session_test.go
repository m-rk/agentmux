package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

func withEnvDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = prev })
	return dir
}

func TestTmuxSocket(t *testing.T) {
	if got, want := tmuxSocket("probe"), "agentmux-probe"; got != want {
		t.Errorf("tmuxSocket(probe) = %q, want %q", got, want)
	}
}

func TestSessionNameOf(t *testing.T) {
	cases := []struct {
		name     string
		fields   map[string]string
		fallback string
		want     string
	}{
		{
			name:     "prefers AGENTMUX_TMUX_SESSION_NAME",
			fields:   map[string]string{"AGENTMUX_TMUX_SESSION_NAME": "tmux-name", "AGENTMUX_SESSION_NAME": "session-name"},
			fallback: "fallback",
			want:     "tmux-name",
		},
		{
			name:     "falls back to AGENTMUX_SESSION_NAME",
			fields:   map[string]string{"AGENTMUX_SESSION_NAME": "session-name"},
			fallback: "fallback",
			want:     "session-name",
		},
		{
			name:     "falls back to the fallback when neither is set",
			fields:   map[string]string{},
			fallback: "fallback",
			want:     "fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionNameOf(tc.fields, tc.fallback); got != tc.want {
				t.Errorf("sessionNameOf(%v, %q) = %q, want %q", tc.fields, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestAgentOf(t *testing.T) {
	if got, want := agentOf(map[string]string{"AGENTMUX_AGENT": "zero"}), "zero"; got != want {
		t.Errorf("agentOf(zero) = %q, want %q", got, want)
	}
	// backends/claude-code never sets AGENTMUX_AGENT — an absent field means
	// claude-code, matching discovery.go's own default.
	if got, want := agentOf(map[string]string{}), "claude-code"; got != want {
		t.Errorf("agentOf(empty) = %q, want %q", got, want)
	}
}

func TestAgentFor(t *testing.T) {
	dir := withEnvDir(t)

	if err := os.WriteFile(filepath.Join(dir, "probe-zero.env"), []byte("AGENTMUX_AGENT=zero\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent, err := agentFor("probe-zero")
	if err != nil {
		t.Fatalf("agentFor: %v", err)
	}
	if agent != "zero" {
		t.Errorf("agentFor(probe-zero) = %q, want %q", agent, "zero")
	}

	if _, err := agentFor("does-not-exist"); err == nil {
		t.Error("agentFor(does-not-exist) = nil error, want an error for a missing registry file")
	}
}

func TestRegistry(t *testing.T) {
	dir := withEnvDir(t)
	content := "" +
		"AGENTMUX_INSTANCE_NAME=probe\n" +
		"# comment\n" +
		"\n" +
		"AGENTMUX_WORKDIR=/w\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, err := registry("probe")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if fields["AGENTMUX_INSTANCE_NAME"] != "probe" || fields["AGENTMUX_WORKDIR"] != "/w" {
		t.Errorf("fields = %v, missing expected keys", fields)
	}
	if len(fields) != 2 {
		t.Errorf("fields = %v, want exactly 2 entries (comment/blank line should be skipped)", fields)
	}
}
