package main

import (
	"strings"
	"testing"
)

func TestFormatCodexAuthStatus(t *testing.T) {
	in := formatAuthStatus(authStatusResult{Agent: "codex", RunUser: "alice", LoggedIn: true, AuthMethod: "chatgpt"}, "")
	if in != "codex alice: logged in (method chatgpt)" {
		t.Errorf("logged in = %q", in)
	}
	out := formatAuthStatus(authStatusResult{Agent: "codex", RunUser: "alice"}, "")
	if !strings.Contains(out, "NOT logged in") || !strings.Contains(out, "--device-auth") {
		t.Errorf("logged out = %q", out)
	}
	key := formatAuthStatus(authStatusResult{Agent: "codex", RunUser: "alice", LoggedIn: true, AuthMethod: "api-key"}, "")
	if !strings.Contains(key, "API-key billing") {
		t.Errorf("api key = %q", key)
	}
}
