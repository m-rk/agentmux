package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaudeAuth returns a command factory for a fake `claude auth status`
// that prints the given outputs in turn (the last repeats), counting calls.
func fakeClaudeAuth(t *testing.T, outputs ...string) (func() *exec.Cmd, func() int) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	for i, o := range outputs {
		if err := os.WriteFile(filepath.Join(dir, "out"+string(rune('0'+i))), []byte(o), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `n=$(cat "$1/count" 2>/dev/null || echo 0); echo $((n+1)) > "$1/count"
f="$1/out$n"; [ -f "$f" ] || f="$1/out` + string(rune('0'+len(outputs)-1)) + `"; cat "$f"; grep -q '^EXIT' "$f" && exit 1; exit 0`
	calls := func() int {
		b, _ := os.ReadFile(counter)
		return int(strings.TrimSpace(string(b))[0] - '0')
	}
	return func() *exec.Cmd { return exec.Command("sh", "-c", script, "sh", dir) }, calls
}

func TestCheckClaudeLoginRetriesOnce(t *testing.T) {
	old := claudeLoginRetryDelay
	claudeLoginRetryDelay = time.Millisecond
	defer func() { claudeLoginRetryDelay = old }()

	t.Run("fails once then passes", func(t *testing.T) {
		cmd, calls := fakeClaudeAuth(t, `{"loggedIn":false}`, `{"loggedIn":true}`)
		if err := checkClaudeLogin(` for user "alice"`, cmd); err != nil {
			t.Fatalf("want success after retry, got %v", err)
		}
		if calls() != 2 {
			t.Fatalf("want 2 checks, got %d", calls())
		}
	})
	t.Run("passes first time without waiting", func(t *testing.T) {
		cmd, calls := fakeClaudeAuth(t, `{"loggedIn":true}`)
		if err := checkClaudeLogin("", cmd); err != nil || calls() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls())
		}
	})
	t.Run("fails twice and says what it saw", func(t *testing.T) {
		cmd, calls := fakeClaudeAuth(t, `{"loggedIn":false}`)
		err := checkClaudeLogin(` for user "alice"`, cmd)
		if err == nil || calls() != 2 {
			t.Fatalf("err=%v calls=%d", err, calls())
		}
		for _, want := range []string{`user "alice"`, "claude auth status --json", "loggedIn=false", "checked twice", "credentials refresh"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})
	t.Run("unparseable output is named", func(t *testing.T) {
		cmd, _ := fakeClaudeAuth(t, "not json")
		err := checkClaudeLogin("", cmd)
		if err == nil || !strings.Contains(err.Error(), "unparseable output (not json)") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("command failure is named", func(t *testing.T) {
		cmd, _ := fakeClaudeAuth(t, "EXIT")
		err := checkClaudeLogin("", cmd)
		if err == nil || !strings.Contains(err.Error(), "failed (exit status 1") {
			t.Fatalf("got %v", err)
		}
	})
}
