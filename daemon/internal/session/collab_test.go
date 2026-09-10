package session

import "testing"

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
	if collaborationPaneSafe("claude-code", "shell prompt only") {
		t.Fatal("Claude pane without /rc was considered safe")
	}
	if !collaborationPaneSafe("kilo", "Ask anything\nctrl+p commands\n◆ Remote") {
		t.Fatal("ready Kilo pane was considered unsafe")
	}
}
