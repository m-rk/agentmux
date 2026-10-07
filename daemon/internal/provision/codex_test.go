package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fakeCodexLogin(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exec.Command(path, "login", "status")
}

func TestCodexLoginStatusClassifiesWithoutEchoing(t *testing.T) {
	cases := []struct {
		name, script, method string
		loggedIn             bool
	}{
		{"chatgpt", `echo "Logged in using ChatGPT"`, "chatgpt", true},
		{"api key", `echo "Logged in using an API key - sk-SECRETFRAGMENT***"`, "api-key", true},
		{"logged out", `echo "Not logged in" >&2; exit 1`, "", false},
	}
	for _, c := range cases {
		in, method, err := codexLoginStatusVia(fakeCodexLogin(t, c.script))
		if err != nil || in != c.loggedIn || method != c.method {
			t.Errorf("%s: got (%v, %q, %v), want (%v, %q, nil)", c.name, in, method, err, c.loggedIn, c.method)
		}
		if strings.Contains(method, "SECRET") {
			t.Errorf("%s: method leaks output", c.name)
		}
	}
	if _, _, err := codexLoginStatusVia(exec.Command("/nonexistent/codex", "login", "status")); err == nil {
		t.Error("missing binary should be an error, not logged out")
	}
}

// The login check runs `codex login status` and nothing else: an auth.json
// under CODEX_HOME is never opened, so its contents cannot surface.
func TestCodexLoginStatusDoesNotReadAuthFiles(t *testing.T) {
	home := t.TempDir()
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"access_token":"SENTINEL-TOKEN"}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	in, method, err := codexLoginStatusVia(fakeCodexLogin(t, `echo "Logged in using ChatGPT"`))
	if err != nil || !in || method != "chatgpt" {
		t.Fatalf("got (%v, %q, %v)", in, method, err)
	}
}
