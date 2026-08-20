package session

import (
	"os/exec"
	"strings"
	"testing"
)

// TestUpdateAgentOpencodeDoesNotShellOutThroughItself guards against a real
// incident: `opencode upgrade --method npm` shells out through the
// currently-installed opencode binary to perform its own upgrade, so once
// that binary is broken (confirmed live: npm's postinstall silently failed
// to link the platform binary), every future refresh just re-triggers the
// same exec failure forever with no way to self-heal. Installing the npm
// package directly needs nothing from the existing binary.
func TestUpdateAgentOpencodeDoesNotShellOutThroughItself(t *testing.T) {
	var gotName string
	var gotArgs []string
	previousRunAs := runAs
	runAs = func(runUser, name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string{}, args...)
		return exec.Command("true")
	}
	t.Cleanup(func() { runAs = previousRunAs })

	if err := updateAgent("someuser", "opencode", nil); err != nil {
		t.Fatalf("updateAgent: %v", err)
	}
	if gotName == "opencode" {
		t.Fatalf("updateAgent invoked the opencode binary itself (%s %v); it must not depend on the binary it's trying to fix", gotName, gotArgs)
	}
	if gotName != "npm" || strings.Join(gotArgs, " ") != "install -g opencode-ai@latest" {
		t.Errorf("updateAgent for opencode = %s %v, want npm install -g opencode-ai@latest", gotName, gotArgs)
	}
}
