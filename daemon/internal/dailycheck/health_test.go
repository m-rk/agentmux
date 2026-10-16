package dailycheck

import (
	"context"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func TestCommandsAreCodexIdle(t *testing.T) {
	idle := []string{"sh -c while :; do sleep 3600; done codex-idle", "sleep 3600"}
	if !commandsAreCodexIdle(idle) {
		t.Error("idle placeholder tree not detected")
	}
	running := append(append([]string{}, idle...), "/home/u/.npm-global/bin/codex exec task")
	if commandsAreCodexIdle(running) {
		t.Error("tree with live codex treated as placeholder")
	}
	if commandsAreCodexIdle([]string{"bash"}) {
		t.Error("plain shell treated as placeholder")
	}
}

func TestCodexIdlePlaceholderSkipsPaneEmpty(t *testing.T) {
	prev := codexIdlePlaceholder
	t.Cleanup(func() { codexIdlePlaceholder = prev })
	for _, idle := range []bool{true, false} {
		codexIdlePlaceholder = func(context.Context, *pb.Instance) bool { return idle }
		client := &fakeClient{instances: []*pb.Instance{{Name: "cx", Agent: "codex", TmuxSession: "s", Pid: 5, Status: pb.Status_STATUS_RUNNING}}}
		_, issues, err := collectHealth(context.Background(), client, Options{MaxPaneBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
		has := false
		for _, i := range issues {
			has = has || i.Code == "pane-empty"
		}
		if has == idle {
			t.Errorf("idle=%v: pane-empty present=%v", idle, has)
		}
	}
}
