package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// TestRunTaskChildRefusesLivePost is the AMUX-34 integration test: it
// starts a run through the same code `sessions run` uses, with a fake
// `amp` binary that — after printing the stream init record — execs a
// real `agentmux asks post` (dry-run off) against a fake Discord gateway.
// The post must be refused by the live guard, with the fake gateway
// untouched. No environment is set by hand for the child: its cwd is a
// neutral temp dir, so the refusal can only come from the identity
// `sessions run` stamps on the run child (see session.AmpRunEnv).
func TestRunTaskChildRefusesLivePost(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH: cannot build the agentmux binary under test")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc (non-Linux): the run spawner is Linux-only")
	}
	agentmuxBin := buildAgentmux(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	workdir := filepath.Join(root, "work")
	binDir := filepath.Join(home, ".local", "bin")
	cfgDir := filepath.Join(home, ".config", "agentmux")
	for _, d := range []string{home, workdir, binDir, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	withTestHostModeAt(t, home)
	// The child must carry only what the run stamps: clear any ambient
	// identity (and the override) from this test process.
	t.Setenv("AGENTMUX_INSTANCE_NAME", "")
	t.Setenv("AGENTMUX_TASK_SESSION", "")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "")

	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldEnvDir := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = oldEnvDir })
	reg := "AGENTMUX_AGENT=amp\nAGENTMUX_WORKDIR=" + workdir + "\nAGENTMUX_AMP_RUNNER_ID=probe\n"
	if err := os.WriteFile(filepath.Join(envDir, "task-9.env"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var gatewayHits []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gatewayHits = append(gatewayHits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/channels/forum" {
			forum := map[string]any{"id": "forum", "guild_id": "guild", "type": 15,
				"available_tags": []map[string]string{{"id": "t-ask", "name": "ask"}, {"id": "t-pending", "name": "pending"}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(forum)
			return
		}
		http.NotFound(w, r)
	}))
	defer gw.Close()
	cfg := "bot_token: test-token\ncollaboration:\n  bot_token: test-token\n  webhook_url: " + gw.URL + "/hook\n" +
		"  forum_channel_id: forum\n  ask_mention_user_id: \"777\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "discord.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	bodyPath := filepath.Join(root, "body.md")
	if err := os.WriteFile(bodyPath, []byte("probe body from inside a task run"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(root, "verdict")
	// The fake amp: print the stream init record stdout lands in the run
	// log, then run the real CLI posting path in this exact environment.
	fakeAmp := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"T-99999999-9999-4999-8999-999999999999\"}'\n" +
		"\"" + agentmuxBin + "\" asks post -title \"probe\" -body-file \"" + bodyPath + "\" >\"" + verdictPath + ".log\" 2>&1\n" +
		"echo \"exit=$?\" >\"" + verdictPath + "\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "amp"), []byte(fakeAmp), 0o755); err != nil {
		t.Fatal(err)
	}

	addr := "task-9@" + address.LocalHostName()
	res, rerr := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "do the thing"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want ok", res)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(verdictPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake amp never finished the asks post")
		}
		time.Sleep(200 * time.Millisecond)
	}
	verdict, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(verdict)) == "exit=0" {
		t.Fatalf("asks post inside a task run succeeded: %s", verdict)
	}
	log, err := os.ReadFile(verdictPath + ".log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), liveguard.Refusal) {
		t.Fatalf("asks post refused without the guard message: %q", log)
	}
	mu.Lock()
	hits := append([]string(nil), gatewayHits...)
	mu.Unlock()
	for _, h := range hits {
		if strings.HasSuffix(h, "/hook") {
			t.Fatalf("fake gateway was hit: %v", hits)
		}
	}
}

// TestRunTaskChildRoutesPostIntoTestThread is the AMUX-39 integration
// test: the same harness as TestRunTaskChildRefusesLivePost, but with a
// test thread configured. The task run child's `agentmux asks post`
// (dry-run off) must succeed as a reply into the test thread — one bot
// message POST, no forum post, no webhook hit. The old
// AGENTMUX_ALLOW_LIVE=1 override is set on purpose and must change
// nothing.
func TestRunTaskChildRoutesPostIntoTestThread(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH: cannot build the agentmux binary under test")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc (non-Linux): the run spawner is Linux-only")
	}
	agentmuxBin := buildAgentmux(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	workdir := filepath.Join(root, "work")
	binDir := filepath.Join(home, ".local", "bin")
	cfgDir := filepath.Join(home, ".config", "agentmux")
	for _, d := range []string{home, workdir, binDir, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	// The child must carry only what the run stamps: clear any ambient
	// identity from this test process. The API base routes the bot calls
	// at the fake gateway; the dead override is set to prove it is dead.
	t.Setenv("AGENTMUX_INSTANCE_NAME", "")
	t.Setenv("AGENTMUX_TASK_SESSION", "")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "1")

	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldEnvDir := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = oldEnvDir })
	reg := "AGENTMUX_AGENT=amp\nAGENTMUX_WORKDIR=" + workdir + "\nAGENTMUX_AMP_RUNNER_ID=probe\n"
	if err := os.WriteFile(filepath.Join(envDir, "task-9.env"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var gatewayHits []string
	var testPosts []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gatewayHits = append(gatewayHits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/channels/forum":
			forum := map[string]any{"id": "forum", "guild_id": "guild", "type": 15,
				"available_tags": []map[string]string{{"id": "t-ask", "name": "ask"}, {"id": "t-pending", "name": "pending"}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(forum)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/channels/4242/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			testPosts = append(testPosts, body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "501", "channel_id": "4242"})
			return
		}
		http.NotFound(w, r)
	}))
	defer gw.Close()
	t.Setenv("AGENTMUX_DISCORD_API_BASE", gw.URL+"/api")
	cfg := "bot_token: test-token\ncollaboration:\n  bot_token: test-token\n  webhook_url: " + gw.URL + "/hook\n" +
		"  forum_channel_id: forum\n  ask_mention_user_id: \"777\"\n  test_thread: \"4242\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "discord.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	bodyPath := filepath.Join(root, "body.md")
	if err := os.WriteFile(bodyPath, []byte("probe body from inside a task run"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(root, "verdict")
	// The fake amp: print the stream init record stdout lands in the run
	// log, then run the real CLI posting path in this exact environment.
	fakeAmp := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"T-99999999-9999-4999-8999-999999999999\"}'\n" +
		"\"" + agentmuxBin + "\" asks post -title \"probe\" -body-file \"" + bodyPath + "\" >\"" + verdictPath + ".log\" 2>&1\n" +
		"echo \"exit=$?\" >\"" + verdictPath + "\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "amp"), []byte(fakeAmp), 0o755); err != nil {
		t.Fatal(err)
	}

	addr := "task-9@" + address.LocalHostName()
	// AMUX-36: every amp run needs a host mode; pass a fake one the way
	// other run tests do, so the run reaches the fake amp.
	res, rerr := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "do the thing", Mode: "test-mode"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want ok", res)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(verdictPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake amp never finished the asks post")
		}
		time.Sleep(200 * time.Millisecond)
	}
	verdict, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(verdict)) != "exit=0" {
		t.Fatalf("asks post inside a task run failed: %s", verdict)
	}
	log, err := os.ReadFile(verdictPath + ".log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(log), "test thread: ") {
		t.Fatalf("asks post did not route into the test thread: %q", log)
	}
	mu.Lock()
	hits := append([]string(nil), gatewayHits...)
	posts := append([]map[string]any(nil), testPosts...)
	mu.Unlock()
	for _, h := range hits {
		if strings.HasSuffix(h, "/hook") || strings.HasSuffix(h, "/threads") {
			t.Fatalf("live forum write from a task session: %v", hits)
		}
	}
	if len(posts) != 1 {
		t.Fatalf("test posts = %d, want 1: %v", len(posts), hits)
	}
}

// buildAgentmux compiles the real CLI once for the integration test, so
// the fake amp runs the genuine `asks post` path — flag parsing,
// config, guard and all — rather than a reimplementation of it.
func buildAgentmux(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no caller")
	}
	daemonDir := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	out := filepath.Join(t.TempDir(), "agentmux")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/agentmux")
	cmd.Dir = daemonDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building agentmux: %v: %s", err, out)
	}
	return out
}

// TestRunStampsTaskInstanceOnSpawn pins that a task-* run stamps its own
// instance on the spawn: the fake records it (see session.AmpFakeSpawn).
func TestRunStampsTaskInstanceOnSpawn(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	workdir := filepath.Join(root, "work")
	for _, d := range []string{home, workdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	withTestHostModeAt(t, home)
	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldEnvDir := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = oldEnvDir })
	reg := "AGENTMUX_AGENT=amp\nAGENTMUX_WORKDIR=" + workdir + "\nAGENTMUX_AMP_RUNNER_ID=probe\n"
	if err := os.WriteFile(filepath.Join(envDir, "task-9.env"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	id := "T-44444444-4444-4444-8444-444444444444"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	addr := "task-9@" + address.LocalHostName()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if fake.Instance != "task-9" {
		t.Fatalf("spawn instance = %q, want task-9", fake.Instance)
	}
}
