package main

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestVersionCommandHeadless(t *testing.T) {
	cmd := versionCommand(t, "version")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("agentmux version: %v", err)
	}
	if !regexp.MustCompile(`\([0-9a-f]{40}\)`).Match(out) {
		t.Fatalf("version output does not contain a full commit: %q", out)
	}
}

func TestVersionCommandJSON(t *testing.T) {
	cmd := versionCommand(t, "--version", "-json")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("agentmux --version -json: %v", err)
	}
	var got versionInfo
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON output %q: %v", out, err)
	}
	if got.Version == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(got.Commit) {
		t.Fatalf("version info = %+v", got)
	}
}

func versionCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	sha, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("reading current commit: %v", err)
	}
	ldflags := "-X main.commit=" + strings.TrimSpace(string(sha))
	cmdArgs := append([]string{"run", "-ldflags", ldflags, "."}, args...)
	cmd := exec.Command("go", cmdArgs...)
	cmd.Stdin = strings.NewReader("")
	return cmd
}
