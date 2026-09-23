package claudeauth

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeClaudeBody is consumed by a fake `claude` executable on PATH: the
// helper prints body to stdout and exits with the embedded code.
const fakeClaudeBodyEnv = "AGENTMUX_TEST_CLAUDE_BODY"

func installFakeClaude(t *testing.T, body string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shell script is POSIX-only")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' \"$AGENTMUX_TEST_CLAUDE_BODY\"\nexit " + fmt.Sprint(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Isolate HOME so runas.CurrentUserCommand's fixed-up PATH (which
	// prepends $HOME/.local/bin and friends) can't resolve a real `claude`
	// ahead of the fake on PATH — confirmed live: without this the test
	// executed the operator's real logged-in claude instead of the fake.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(fakeClaudeBodyEnv, body)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestLoginMethodArgs(t *testing.T) {
	def, err := LoginMethodArgs("")
	if err != nil {
		t.Fatalf("default method: %v", err)
	}
	if len(def) != 1 || def[0] != "--claudeai" {
		t.Fatalf("default method = %v, want [--claudeai]", def)
	}
	console, err := LoginMethodArgs("console")
	if err != nil {
		t.Fatalf("console method: %v", err)
	}
	if len(console) != 1 || console[0] != "--console" {
		t.Fatalf("console method = %v, want [--console]", console)
	}
	if _, err := LoginMethodArgs("sso"); err == nil {
		t.Fatal("unknown method should error")
	}
}

func TestStartLoginHeadlessEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "placeholder-should-be-stripped")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "should-be-stripped")
	t.Setenv("TERM", "")
	cmd, err := StartLogin("nobody-user-for-test", "claudeai")
	if err != nil {
		// Non-root test runners cannot privilege-drop to another user;
		// the production path is covered by the error branch below.
		if os.Geteuid() != 0 {
			t.Skipf("StartLogin as another user requires root: %v", err)
		}
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Env, "\n")
	if strings.Contains(joined, "ANTHROPIC_API_KEY=") || strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=") {
		t.Fatalf("auth token env leaked into login child:\n%s", RedactLoginURLs(joined))
	}
	if !strings.Contains(joined, "BROWSER=true") {
		t.Fatal("BROWSER=true missing from login child env")
	}
}

func TestCheckLoggedInTrustsBodyOverExitCode(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skipf("resolving current user: %v", err)
	}
	installFakeClaude(t, `{"loggedIn":false,"authMethod":"oauth"}`, 1)
	loggedIn, method, err := CheckLoggedIn(cur.Username)
	if err != nil {
		t.Fatalf("logged-out (exit 1) with valid body should not error: %v", err)
	}
	if loggedIn || method != "oauth" {
		t.Fatalf("got loggedIn=%v method=%q, want false/oauth", loggedIn, method)
	}

	installFakeClaude(t, `{"loggedIn":true,"authMethod":"oauth"}`, 0)
	loggedIn, _, err = CheckLoggedIn(cur.Username)
	if err != nil || !loggedIn {
		t.Fatalf("logged-in should parse cleanly: loggedIn=%v err=%v", loggedIn, err)
	}

	installFakeClaude(t, `not json at all`, 1)
	if _, _, err := CheckLoggedIn(cur.Username); err == nil {
		t.Fatal("unparseable output should error")
	}
}

func TestExtractLoginURL(t *testing.T) {
	// Plain output, as seen without terminal escapes.
	plain := "visit:\nhttps://claude.com/cai/oauth/authorize?code=true&client_id=abc&code_challenge=xyz&state=ABCDEFGH\nEnter the code"
	if got := ExtractLoginURL(plain); !strings.HasPrefix(got, "https://claude.com/cai/oauth/authorize?code=true") {
		t.Fatalf("plain URL not extracted: %q", got)
	}
	// OSC-8 hyperlink wrapper, as seen under a PTY: ESC ] 8 ;; URL BEL label
	// ESC ] 8 ;; BEL. The URL must not swallow escape bytes or the label.
	wrapped := "\x1b]8;;https://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=XYZ\x07https://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=XYZ\x1b]8;;\x07"
	got := ExtractLoginURL(wrapped)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("escape bytes leaked into URL: %q", got)
	}
	if !strings.HasSuffix(got, "state=XYZ") {
		t.Fatalf("wrapped URL truncated wrongly: %q", got)
	}
	if ExtractLoginURL("no URL here yet") != "" {
		t.Fatal("missing URL should return empty string")
	}
}

func TestRedactLoginURLs(t *testing.T) {
	in := "visit https://claude.com/cai/oauth/authorize?code=true&state=SECRET then paste"
	redacted := RedactLoginURLs(in)
	if strings.Contains(redacted, "SECRET") {
		t.Fatalf("query state leaked: %q", redacted)
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("placeholder missing: %q", redacted)
	}
}

func TestOutputScanners(t *testing.T) {
	if !PromptForCode("Paste code here if prompted > ") {
		t.Fatal("code prompt not detected")
	}
	if PromptForCode("waiting for browser...") {
		t.Fatal("false positive on code prompt")
	}
	if !InvalidCodeReported("Invalid code. Please make sure the full code was copied.") {
		t.Fatal("invalid-code rejection not detected")
	}
	if !LoginSuccessful("Login successful. Welcome back!") {
		t.Fatal("success line not detected")
	}
}
