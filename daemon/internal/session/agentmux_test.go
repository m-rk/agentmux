package session

import (
	"encoding/json"
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

	if _, err := kiloInstanceXDGEnv("probe"); err == nil {
		t.Fatal("kiloInstanceXDGEnv with a ready marker but no account login: want an error, got none")
	} else if !strings.Contains(err.Error(), "account login") {
		t.Fatalf("kiloInstanceXDGEnv with no account login: want an error about missing login, got %v", err)
	}

	authDir := filepath.Join(root, "data", "kilo")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte("{}"), 0o600); err != nil {
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

// TestWriteOpencodeConfigPreservesHandAddedModels guards against
// regenerating opencode.json wiping models a user added by hand: confirmed
// live that a plain restart clobbered them back down to just the
// instance's own default model.
func TestWriteOpencodeConfigPreservesHandAddedModels(t *testing.T) {
	workdir := t.TempDir()
	path := filepath.Join(workdir, "opencode.json")
	existing := `{
		"provider": {
			"my-gateway": {
				"models": {
					"glm-5.2": {"name": "glm-5.2"},
					"kimi-k2.6": {"name": "kimi-k2.6"}
				}
			}
		}
	}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeOpencodeConfig("my-gateway", "glm-5.2", "https://gateway.example/v1", "", workdir); err != nil {
		t.Fatalf("writeOpencodeConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Provider map[string]struct {
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshaling written config: %v", err)
	}
	models := doc.Provider["my-gateway"].Models
	if _, ok := models["kimi-k2.6"]; !ok {
		t.Errorf("writeOpencodeConfig dropped the hand-added model kimi-k2.6, got models: %v", models)
	}
	if _, ok := models["glm-5.2"]; !ok {
		t.Errorf("writeOpencodeConfig dropped its own default model glm-5.2, got models: %v", models)
	}
}

// TestConfigureAgentIfChangedPreservesInAppModelSwitch guards against the
// bug this session diagnosed live: RunAgentmux used to call
// configureAgent unconditionally on every restart, so switching models
// inside the running opencode session (which rewrites the same
// opencode.json) got silently reverted back to the registry's configured
// default the moment the instance next restarted.
func TestConfigureAgentIfChangedPreservesInAppModelSwitch(t *testing.T) {
	dir := withEnvDir(t)
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte("AGENTMUX_INSTANCE_NAME=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	path := filepath.Join(workdir, "opencode.json")

	fields := map[string]string{}
	if err := configureAgentIfChanged("probe", fields, "opencode", "my-gateway", "glm-5.2", "https://gateway.example/v1", "", workdir); err != nil {
		t.Fatalf("initial configureAgentIfChanged: %v", err)
	}
	fields, err := registry("probe")
	if err != nil {
		t.Fatal(err)
	}
	if fields["AGENTMUX_LAST_CONFIG_HASH"] == "" {
		t.Fatal("configureAgentIfChanged did not record AGENTMUX_LAST_CONFIG_HASH after its first write")
	}

	// Simulate the running agent's own in-app model switch: it rewrites the
	// same file's top-level "model" field directly.
	if err := os.WriteFile(path, []byte(`{"model":"my-gateway/MiniMax-M3","provider":{"my-gateway":{"models":{"glm-5.2":{"name":"glm-5.2"}}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same registry fields, same provider/model args as before (the ordinary
	// restart case) -- must NOT stomp the in-app switch back to glm-5.2.
	if err := configureAgentIfChanged("probe", fields, "opencode", "my-gateway", "glm-5.2", "https://gateway.example/v1", "", workdir); err != nil {
		t.Fatalf("second configureAgentIfChanged: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model != "my-gateway/MiniMax-M3" {
		t.Fatalf("configureAgentIfChanged stomped the in-app model switch: model = %q, want my-gateway/MiniMax-M3 preserved", doc.Model)
	}

	// Now simulate an explicit `agentmux new -y -model ...`: the registry's
	// own model field changes, so the next restart SHOULD apply it.
	if err := configureAgentIfChanged("probe", fields, "opencode", "my-gateway", "MiniMax-M3", "https://gateway.example/v1", "", workdir); err != nil {
		t.Fatalf("third configureAgentIfChanged: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model != "my-gateway/MiniMax-M3" {
		t.Fatalf("configureAgentIfChanged did not apply an explicit registry model change: model = %q, want my-gateway/MiniMax-M3", doc.Model)
	}
}

// TestConfigureAgentIfChangedDegradesGracefullyOnRegistryPermissionError
// guards against a fleet-wide incident: a registry file whose ownership
// didn't allow the tick's own user to write it (root-owned from
// provisioning, tick running as the instance's run user — see
// provision.chownRegistryForUser) made SetRegistryField's hash-caching
// write fail with EACCES. Before this test's fix, that error propagated
// out of configureAgentIfChanged and failed the whole RunAgentmux tick
// before it ever reached the hasSession/session-start check below it,
// taking down every affected instance's actual liveness check over a
// side-channel write it didn't need. Losing the cached hash must instead
// be a logged no-op: the agent config still gets written, and the tick
// still proceeds.
func TestConfigureAgentIfChangedDegradesGracefullyOnRegistryPermissionError(t *testing.T) {
	dir := withEnvDir(t)
	regPath := filepath.Join(dir, "probe.env")
	if err := os.WriteFile(regPath, []byte("AGENTMUX_INSTANCE_NAME=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()

	// Simulate the registry file being unwritable by the current process.
	if err := os.Chmod(regPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(regPath, 0o644) })

	fields := map[string]string{}
	if err := configureAgentIfChanged("probe", fields, "opencode", "my-gateway", "glm-5.2", "https://gateway.example/v1", "", workdir); err != nil {
		t.Fatalf("configureAgentIfChanged with an unwritable registry file = %v, want nil (must degrade gracefully)", err)
	}

	if _, err := os.Stat(filepath.Join(workdir, "opencode.json")); err != nil {
		t.Fatalf("configureAgentIfChanged did not write the agent config despite the registry write failure: %v", err)
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
