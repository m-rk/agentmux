package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCollaborationPaneSafe(t *testing.T) {
	for _, test := range []struct {
		agent string
		pane  string
	}{
		{"claude-code", "Do you want to allow this tool?\n❯ Yes\n  No\n/rc"},
		{"claude-code", "Disconnect this session\nEnter to select · Esc to continue\n/rc"},
		{"kilo", "Add credential\nctrl+p commands\n◆ Remote"},
		{"claude-code", "Working…\nEsc to cancel\n/rc"},
	} {
		if collaborationPaneSafe(test.agent, test.pane) {
			t.Fatalf("interactive pane was considered safe:\n%s", test.pane)
		}
	}
	if pane := "Useful response complete.\n\n❯ \n/rc"; !collaborationPaneSafe("claude-code", pane) {
		t.Fatalf("idle pane was considered unsafe:\n%s", pane)
	}
	// ClaudePaneRemoteConnected is currently always true (see its doc
	// comment: claude-code 2.1.271 shows no known indicator even when
	// genuinely connected), so a plain idle-looking pane with no /rc and no
	// dialog markers is correctly treated as safe rather than unsafe.
	if !collaborationPaneSafe("claude-code", "shell prompt only") {
		t.Fatal("idle Claude pane without /rc was considered unsafe")
	}
	if !collaborationPaneSafe("kilo", "Ask anything\nctrl+p commands\n◆ Remote") {
		t.Fatal("ready Kilo pane was considered unsafe")
	}
}

// collabFakeTmux records calls and serves pane captures from a queue; the last
// capture repeats.
type collabFakeTmux struct {
	calls    []string
	captures []string
	loaded   string
}

func (f *collabFakeTmux) cmd(args ...string) *exec.Cmd {
	f.calls = append(f.calls, strings.Join(args[2:], " "))
	cmd := exec.Command("sh", "-c", `cat >/dev/null; printf '%s' "$FAKE_OUT"`)
	switch args[2] {
	case "capture-pane":
		out := f.captures[0]
		if len(f.captures) > 1 {
			f.captures = f.captures[1:]
		}
		cmd.Env = append(os.Environ(), "FAKE_OUT="+out)
	case "load-buffer":
		cmd = exec.Command("sh", "-c", "cat")
		cmd.Stdout = nil
	}
	return cmd
}

func TestDeliverCollabPrompt(t *testing.T) {
	old := collabSubmitSettle
	collabSubmitSettle = time.Millisecond
	defer func() { collabSubmitSettle = old }()

	f := &collabFakeTmux{captures: []string{"[Pasted text #1 +9 lines]\n❯ [Pasted text #1 +9 lines]\n", "done\n❯ \n"}}
	if err := deliverCollabPrompt(f.cmd, "sock", "sess", "claude-code", "a\nb"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "|")
	if strings.Contains(joined, "send-keys -t sess -l") {
		t.Fatalf("prompt was typed with send-keys -l: %s", joined)
	}
	if !strings.Contains(joined, "paste-buffer -p -d") {
		t.Fatalf("no bracketed paste: %s", joined)
	}
	if n := strings.Count(joined, "send-keys -t sess Enter"); n != 2 {
		t.Fatalf("Enter should be retried while draft remains, got %d: %s", n, joined)
	}

	stuck := &collabFakeTmux{captures: []string{"❯ [Pasted text #1 +9 lines]\n"}}
	if err := deliverCollabPrompt(stuck.cmd, "sock", "sess", "claude-code", "a\nb"); err == nil {
		t.Fatal("an unsent draft must not count as delivered")
	}
	if !strings.Contains(strings.Join(stuck.calls, "|"), "send-keys -t sess C-c") {
		t.Fatalf("unsent paste was not cleared: %v", stuck.calls)
	}

	// Cleared by the first C-c: error, but no C-u.
	cleared := &collabFakeTmux{captures: []string{"❯ [Pasted text #1 +9 lines]\n", "❯ [Pasted text #1 +9 lines]\n", "❯ [Pasted text #1 +9 lines]\n", "❯ \n"}}
	err := deliverCollabPrompt(cleared.cmd, "sock", "sess", "claude-code", "a\nb")
	if err == nil || !strings.Contains(err.Error(), "cleared") {
		t.Fatalf("want a cleared-and-retry error, got %v", err)
	}
	if strings.Contains(strings.Join(cleared.calls, "|"), "C-u") {
		t.Fatalf("C-u sent after C-c already cleared the draft: %v", cleared.calls)
	}
}
