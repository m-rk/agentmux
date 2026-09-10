package collab

import (
	"os/exec"
	"testing"
)

func TestNormalizeGitRemote(t *testing.T) {
	tests := map[string]string{
		"https://github.com/m-rk/agentmux.git": "github.com/m-rk/agentmux",
		"git@github.com:m-rk/agentmux.git":     "github.com/m-rk/agentmux",
		"ssh://git@GitHub.com/m-rk/AgentMux":   "github.com/m-rk/agentmux",
	}
	for remote, want := range tests {
		got, err := NormalizeGitRemote(remote)
		if err != nil {
			t.Fatalf("NormalizeGitRemote(%q): %v", remote, err)
		}
		if got != want {
			t.Fatalf("NormalizeGitRemote(%q) = %q, want %q", remote, got, want)
		}
	}
	if _, err := NormalizeGitRemote("../local-repo"); err == nil {
		t.Fatal("local origin unexpectedly succeeded")
	}
}

func TestDetectProjectOverride(t *testing.T) {
	got, err := DetectProject("/does/not/matter", "manual/project", func(string, ...string) *exec.Cmd {
		t.Fatal("command should not run for an override")
		return nil
	})
	if err != nil || got != "manual/project" {
		t.Fatalf("DetectProject override = %q, %v", got, err)
	}
}
