package session

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

// withFakeUserLookup stubs userLookup so updateAgent's opencode path (which
// looks up runUser to key the npm install lock by home directory) doesn't
// depend on "someuser" being a real account on whatever machine runs the
// test — see the matching comment on userLookup itself. Uid/Gid are the
// test process's own, so withNpmGlobalLock's chown-if-freshly-created path
// (self-chown) succeeds without needing root.
func withFakeUserLookup(t *testing.T) {
	t.Helper()
	previous := userLookup
	home := t.TempDir()
	userLookup = func(username string) (*user.User, error) {
		return &user.User{
			Username: username,
			HomeDir:  home,
			Uid:      fmt.Sprint(os.Getuid()),
			Gid:      fmt.Sprint(os.Getgid()),
		}, nil
	}
	t.Cleanup(func() { userLookup = previous })
}

// TestUpdateAgentOpencodeDoesNotShellOutThroughItself guards against a real
// incident: `opencode upgrade --method npm` shells out through the
// currently-installed opencode binary to perform its own upgrade, so once
// that binary is broken (confirmed live: npm's postinstall silently failed
// to link the platform binary), every future refresh just re-triggers the
// same exec failure forever with no way to self-heal. Installing the npm
// package directly needs nothing from the existing binary.
func TestUpdateAgentOpencodeDoesNotShellOutThroughItself(t *testing.T) {
	withFakeUserLookup(t)
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

// TestUpdateAgentOpencodeRetriesOnceOnFailure guards against a second real
// incident: postinstall's own fetch of the platform binary is a separate npm
// registry call, and a single transient network failure there is enough to
// fail the whole `npm install -g` and leave the shared global binary as a
// broken stub for every future invocation — including new sessions this
// daemon doesn't manage (confirmed live: Paseo failed against exactly this
// stub minutes after a nightly refresh failed here, and simply re-running
// the same install with no other change succeeded).
func TestUpdateAgentOpencodeRetriesOnceOnFailure(t *testing.T) {
	withFakeUserLookup(t)
	calls := 0
	previousRunAs := runAs
	runAs = func(runUser, name string, args ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return exec.Command("false")
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { runAs = previousRunAs })

	if err := updateAgent("someuser", "opencode", nil); err != nil {
		t.Fatalf("updateAgent: %v", err)
	}
	if calls != 2 {
		t.Fatalf("updateAgent made %d attempts, want 2 (one retry after the first failure)", calls)
	}
}

// TestUpdateAgentOpencodeFailsAfterExhaustingRetry confirms the retry is
// bounded: a persistent failure (not just a one-off blip) must still
// surface as an error rather than retrying forever.
func TestUpdateAgentOpencodeFailsAfterExhaustingRetry(t *testing.T) {
	withFakeUserLookup(t)
	calls := 0
	previousRunAs := runAs
	runAs = func(runUser, name string, args ...string) *exec.Cmd {
		calls++
		return exec.Command("false")
	}
	t.Cleanup(func() { runAs = previousRunAs })

	if err := updateAgent("someuser", "opencode", nil); err == nil {
		t.Fatal("updateAgent: want error after both attempts fail, got nil")
	}
	if calls != 2 {
		t.Fatalf("updateAgent made %d attempts, want exactly 2", calls)
	}
}
