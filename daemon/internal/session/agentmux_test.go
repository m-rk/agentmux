package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKiloInstanceXDGEnvRequiresReadyMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env, err := kiloInstanceXDGEnv("probe")
	if err != nil {
		t.Fatalf("kiloInstanceXDGEnv before migration: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("kiloInstanceXDGEnv before migration = %v, want the legacy shared environment", env)
	}

	root := filepath.Join(home, ".agentmux", "probe", ".kilo-home")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, kiloXDGIsolationReadyFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	env, err = kiloInstanceXDGEnv("probe")
	if err != nil {
		t.Fatalf("kiloInstanceXDGEnv after migration: %v", err)
	}
	want := map[string]string{
		"XDG_DATA_HOME":  filepath.Join(root, "data"),
		"XDG_STATE_HOME": filepath.Join(root, "state"),
	}
	if len(env) != len(want) {
		t.Fatalf("kiloInstanceXDGEnv after migration = %v, want exactly data and state overrides", env)
	}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || want[name] != value {
			t.Errorf("unexpected isolation environment entry %q", entry)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("missing isolation environment entries: %v", want)
	}
	for _, dir := range []string{root, filepath.Join(root, "data"), filepath.Join(root, "state")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode for %s = %#o, want 0700", dir, got)
		}
	}
}

func TestKiloInstanceXDGEnvRejectsNonFileMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := filepath.Join(home, ".agentmux", "probe", ".kilo-home", kiloXDGIsolationReadyFile)
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := kiloInstanceXDGEnv("probe"); err == nil {
		t.Fatal("kiloInstanceXDGEnv accepted a directory as its readiness marker")
	}
}

func TestKiloInstanceXDGEnvRejectsPathTraversal(t *testing.T) {
	if _, err := kiloInstanceXDGEnvForHome("../escape", t.TempDir()); err == nil {
		t.Fatal("kiloInstanceXDGEnvForHome accepted a path traversal instance name")
	}
}

func TestLatestKiloSessionIDUsesProvidedEnv(t *testing.T) {
	workdir := t.TempDir()
	dataHome := filepath.Join(t.TempDir(), "isolated-data")
	previousWithPath := withPath
	withPath = func(string, ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKiloSessionListHelperProcess$")
		cmd.Env = append(os.Environ(),
			"GO_WANT_KILO_SESSION_HELPER=1",
			"GO_KILO_EXPECTED_DATA_HOME="+dataHome,
			"GO_KILO_SESSION_WORKDIR="+workdir,
		)
		return cmd
	}
	t.Cleanup(func() { withPath = previousWithPath })

	id, err := latestKiloSessionID(workdir, []string{"XDG_DATA_HOME=" + dataHome})
	if err != nil {
		t.Fatalf("latestKiloSessionID: %v", err)
	}
	if id != "isolated-session" {
		t.Errorf("latestKiloSessionID = %q, want %q", id, "isolated-session")
	}
}

// TestStopAgentmuxWaitsForSessionToDisappear guards the actual fix: a
// naive StopAgentmux that fired `tmux kill-session` and returned
// immediately let a caller (a plain `systemctl restart`, or
// provision.createAgentmux re-provisioning an existing instance) write
// updated config and launch a replacement process while the old one was
// still exiting — which could then flush its own stale state back to
// disk a moment later, clobbering the update. This confirms StopAgentmux
// actually polls has-session until it reports gone (not just fire the
// kill and return) before applying its fixed grace period.
func TestStopAgentmuxWaitsForSessionToDisappear(t *testing.T) {
	dir := withEnvDir(t)
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte("AGENTMUX_INSTANCE_NAME=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevPoll, prevTimeout, prevGrace := stopSessionPollInterval, stopSessionTimeout, stopSessionGrace
	stopSessionPollInterval = time.Millisecond
	stopSessionTimeout = 200 * time.Millisecond
	stopSessionGrace = 5 * time.Millisecond
	t.Cleanup(func() {
		stopSessionPollInterval, stopSessionTimeout, stopSessionGrace = prevPoll, prevTimeout, prevGrace
	})

	// has-session "still there" for the first two polls, then "gone" —
	// simulating the real-world gap between tmux tearing down its own
	// session bookkeeping and the signaled process actually finishing.
	hasSessionCalls := 0
	prevWithPath := withPath
	withPath = func(name string, args ...string) *exec.Cmd {
		if name != "tmux" {
			t.Fatalf("withPath called with unexpected command %q", name)
		}
		isHasSession := false
		for _, a := range args {
			if a == "has-session" {
				isHasSession = true
			}
		}
		if !isHasSession {
			return exec.Command("true") // kill-session
		}
		hasSessionCalls++
		if hasSessionCalls <= 2 {
			return exec.Command("true") // still there
		}
		return exec.Command("false") // gone
	}
	t.Cleanup(func() { withPath = prevWithPath })

	if err := StopAgentmux("probe"); err != nil {
		t.Fatalf("StopAgentmux: %v", err)
	}
	if hasSessionCalls < 3 {
		t.Errorf("has-session polled %d times, want at least 3 (StopAgentmux must have returned before the session was actually gone)", hasSessionCalls)
	}
}

func TestStopAgentmuxGivesUpAtDeadlineRatherThanHanging(t *testing.T) {
	dir := withEnvDir(t)
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte("AGENTMUX_INSTANCE_NAME=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevPoll, prevTimeout, prevGrace := stopSessionPollInterval, stopSessionTimeout, stopSessionGrace
	stopSessionPollInterval = time.Millisecond
	stopSessionTimeout = 20 * time.Millisecond
	stopSessionGrace = time.Millisecond
	t.Cleanup(func() {
		stopSessionPollInterval, stopSessionTimeout, stopSessionGrace = prevPoll, prevTimeout, prevGrace
	})

	prevWithPath := withPath
	withPath = func(string, ...string) *exec.Cmd { return exec.Command("true") } // has-session always says "still there"
	t.Cleanup(func() { withPath = prevWithPath })

	done := make(chan error, 1)
	go func() { done <- StopAgentmux("probe") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StopAgentmux: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StopAgentmux hung instead of giving up at its deadline")
	}
}

func TestKiloSessionListHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_KILO_SESSION_HELPER") != "1" {
		return
	}
	if got, want := os.Getenv("XDG_DATA_HOME"), os.Getenv("GO_KILO_EXPECTED_DATA_HOME"); got != want {
		fmt.Fprintf(os.Stderr, "XDG_DATA_HOME = %q, want %q", got, want)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stdout, `[{"id":"isolated-session","directory":%q,"updated":1}]`, os.Getenv("GO_KILO_SESSION_WORKDIR"))
	os.Exit(0)
}
