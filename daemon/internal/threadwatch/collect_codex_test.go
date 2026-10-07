package threadwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/session"
)

func TestCodexCollectorMapsRunLog(t *testing.T) {
	home := t.TempDir()
	thread := "00000000-0000-4000-8000-0000000000c3"
	path := session.CodexRunLogPath(home, "cx", thread)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	log := `{"type":"thread.started","thread_id":"` + thread + `"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"agent_message","text":"all done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}
{"type":"turn.failed","error":{"message":"{\"status\":429,\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"usage limit reached\"}}"}}
not json
`
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	offs := newAmpTestOffsets()
	inst := Instance{Name: "cx", Agent: "codex", Home: home}
	evs, err := (&CodexCollector{}).Poll(context.Background(), inst, offs)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
		if e.Thread != thread || e.Agent != "codex" {
			t.Errorf("event %+v lacks thread/agent", e)
		}
	}
	want := []string{KindActivity, KindAssistantMsg, KindTurnEnd, KindUsageLimit}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if evs[2].Tokens.InputTokens != 10 || evs[2].Tokens.CacheReadTokens != 4 {
		t.Errorf("usage = %+v", evs[2].Tokens)
	}
	// A second poll re-emits nothing.
	if evs, _ = (&CodexCollector{}).Poll(context.Background(), inst, offs); len(evs) != 0 {
		t.Errorf("repoll = %v", evs)
	}
}
