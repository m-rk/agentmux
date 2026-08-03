package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
