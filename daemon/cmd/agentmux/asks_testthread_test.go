package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/liveguard"
)

// cliFakeDiscord serves the asks forum and the test thread (id 4242) and
// records every write. The webhook path 404s: task sends must never reach
// it.
type cliFakeDiscord struct {
	mu      sync.Mutex
	posts   []map[string]any
	hooks   int
	patches int
	puts    int
}

func (f *cliFakeDiscord) server(t *testing.T) *httptest.Server {
	t.Helper()
	forum := map[string]any{"id": "forum", "guild_id": "guild", "type": 15,
		"available_tags": []map[string]string{{"id": "t-task", "name": "task"}, {"id": "t-needsme", "name": "needs me"}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/channels/forum":
			_ = json.NewEncoder(w).Encode(forum)
		case r.Method == http.MethodPost && r.URL.Path == "/api/channels/4242/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.posts = append(f.posts, body)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "501", "channel_id": "4242"})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages/"):
			_, _ = w.Write([]byte(`{"id":"501","content":"x","components":[]}`))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/messages/"):
			f.patches++
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/reactions/"):
			f.puts++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/hook":
			f.hooks++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "502", "channel_id": "901"})
		default:
			http.NotFound(w, r)
		}
	}))
}

// taskAsksHome points HOME at a discord.yaml aimed at the fake gateway,
// with or without a test thread, and marks this process a task session.
func taskAsksHome(t *testing.T, gw string, testThread string) {
	t.Helper()
	home := t.TempDir()
	cfg := "collaboration:\n  bot_token: test-token\n  webhook_url: " + gw + "/hook\n" +
		"  forum_channel_id: forum\n  ask_mention_user_id: \"777\"\n"
	if testThread != "" {
		cfg += "  test_thread: \"" + testThread + "\"\n"
	}
	dir := filepath.Join(home, ".config", "agentmux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discord.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENTMUX_DISCORD_API_BASE", gw+"/api")
	t.Setenv(liveguard.TaskEnv, "1")
	t.Setenv(liveguard.InstanceEnv, "")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "1") // must have no effect: the override is gone
}

// captureStdout runs fn with os.Stdout redirected and returns what it
// printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func writeBody(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A task session's post becomes a reply into the test thread and creates
// no forum post — even with the old override set.
func TestTaskPostRoutesIntoTestThread(t *testing.T) {
	f := &cliFakeDiscord{}
	gw := f.server(t)
	defer gw.Close()
	taskAsksHome(t, gw.URL, "4242")
	body := writeBody(t, "render me")
	out, err := captureStdout(t, func() error {
		return runAsksPost([]string{"-title", "probe", "-body-file", body})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("output = %q, want the test-thread routing", out)
	}
	if len(f.posts) != 1 {
		t.Fatalf("posts = %d, want 1 into the test thread", len(f.posts))
	}
}

// Without a test thread the same command refuses — the old override does
// not save it.
func TestTaskPostWithoutTestThreadRefuses(t *testing.T) {
	f := &cliFakeDiscord{}
	gw := f.server(t)
	defer gw.Close()
	taskAsksHome(t, gw.URL, "")
	body := writeBody(t, "render me")
	err := runAsksPost([]string{"-title", "probe", "-body-file", body})
	if err == nil || err.Error() != liveguard.Refusal {
		t.Fatalf("err = %v, want the refusal %q", err, liveguard.Refusal)
	}
	if len(f.posts) != 0 {
		t.Fatalf("posts = %d, want none", len(f.posts))
	}
}

// React and edit act only on test-thread messages; naming a real thread
// is refused.
func TestTaskReactEditOnlyTestThread(t *testing.T) {
	f := &cliFakeDiscord{}
	gw := f.server(t)
	defer gw.Close()
	taskAsksHome(t, gw.URL, "4242")
	if err := runAsksReact([]string{"-thread", "900", "-message", "501", "-emoji", "🤖"}); err == nil {
		t.Fatal("react on a real thread allowed from a task session")
	}
	out, err := captureStdout(t, func() error {
		return runAsksReact([]string{"-thread", "4242", "-message", "501", "-emoji", "🤖"})
	})
	if err != nil || !strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("out = %q err = %v", out, err)
	}
	if f.puts != 1 {
		t.Fatalf("puts = %d", f.puts)
	}
	body := writeBody(t, "edited")
	out, err = captureStdout(t, func() error {
		return runAsksEdit([]string{"-thread", "4242", "-message", "501", "-body-file", body})
	})
	if err != nil || !strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("out = %q err = %v", out, err)
	}
	if f.patches != 1 {
		t.Fatalf("patches = %d", f.patches)
	}
}

// Close, tag and list are dry-run style no-ops from a task session.
func TestTaskCloseTagListAreNoOps(t *testing.T) {
	f := &cliFakeDiscord{}
	gw := f.server(t)
	defer gw.Close()
	taskAsksHome(t, gw.URL, "4242")
	out, err := captureStdout(t, func() error {
		return runAsksClose([]string{"-thread", "900"})
	})
	if err != nil || !strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("close: out = %q err = %v", out, err)
	}
	out, err = captureStdout(t, func() error {
		return runAsksTag([]string{"-thread", "900", "-set", "task,working"})
	})
	if err != nil || !strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("tag: out = %q err = %v", out, err)
	}
	out, err = captureStdout(t, func() error {
		return runAsksList([]string{"-all"})
	})
	if err != nil || !strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("list: out = %q err = %v", out, err)
	}
	if len(f.posts) != 0 || f.patches != 0 || f.puts != 0 {
		t.Fatal("a no-op touched Discord")
	}
}

// A person's session still posts live: the same binary outside a task
// session reaches the forum through the webhook, not the test thread.
func TestPersonPostStaysLive(t *testing.T) {
	f := &cliFakeDiscord{}
	gw := f.server(t)
	defer gw.Close()
	home := t.TempDir()
	cfg := "collaboration:\n  bot_token: test-token\n  webhook_url: " + gw.URL + "/hook\n" +
		"  forum_channel_id: forum\n  ask_mention_user_id: \"777\"\n  test_thread: \"4242\"\n"
	dir := filepath.Join(home, ".config", "agentmux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discord.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENTMUX_DISCORD_API_BASE", gw.URL+"/api")
	t.Setenv(liveguard.TaskEnv, "")
	t.Setenv(liveguard.InstanceEnv, "site-amp")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "")
	// Neutral cwd: this repo's own checkout may sit under a task worktree.
	cwd, _ := os.Getwd()
	plain := t.TempDir()
	if err := os.Chdir(plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	body := writeBody(t, "live ask")
	out, err := captureStdout(t, func() error {
		return runAsksPost([]string{"-title", "live", "-body-file", body})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Ask posted in thread") || strings.HasPrefix(out, "test thread: ") {
		t.Fatalf("output = %q, want the live post", out)
	}
	if f.hooks != 1 || len(f.posts) != 0 {
		t.Fatalf("hooks = %d test-posts = %d, want the forum post through the webhook", f.hooks, len(f.posts))
	}
}

// -unlock is never for workers: a task session is refused before any
// Discord call, on both tag and close.
func TestTaskSessionRefusesUnlock(t *testing.T) {
	f := &cliFakeDiscord{}
	gw := f.server(t)
	defer gw.Close()
	taskAsksHome(t, gw.URL, "4242")
	for name, run := range map[string]func() error{
		"tag":   func() error { return runAsksTag([]string{"-thread", "900", "-set", "task,working", "-unlock"}) },
		"close": func() error { return runAsksClose([]string{"-thread", "900", "-unlock"}) },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "can't unlock") {
			t.Fatalf("%s: err = %v, want the unlock refusal", name, err)
		}
	}
	if len(f.posts) != 0 || f.patches != 0 || f.puts != 0 {
		t.Fatal("a refused unlock touched Discord")
	}
}
