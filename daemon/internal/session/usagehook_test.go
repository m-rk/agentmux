package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every Claude instance's generated settings carry the Stop hook, beside
// whatever else they say.
func TestPrepareClaudeSettingsAddsUsageStopHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := usageHookBinary
	usageHookBinary = func() string { return "/opt/am x/agentmux" }
	defer func() { usageHookBinary = old }()

	path := prepareClaudeSettings("task-p-p-1", filepath.Join(home, "w"), map[string]string{"AGENTMUX_CLAUDE_SPEED": "fast"})
	if path == "" {
		t.Fatal("no settings written")
	}
	data, _ := os.ReadFile(path)
	var doc struct {
		FastMode bool `json:"fastMode"`
		Hooks    struct {
			Stop []struct {
				Hooks []struct{ Type, Command string } `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.FastMode || len(doc.Hooks.Stop) != 1 || len(doc.Hooks.Stop[0].Hooks) != 1 {
		t.Fatalf("settings = %s", data)
	}
	h := doc.Hooks.Stop[0].Hooks[0]
	if h.Type != "command" || h.Command != `'/opt/am x/agentmux' usage-hook claude --instance 'task-p-p-1'` {
		t.Errorf("hook = %+v", h)
	}
}

func TestCodexArgsModelEffort(t *testing.T) {
	argv, err := CodexRunArgs(CodexRunOptions{Model: "model-b", Effort: "high"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if m, e := codexArgsModelEffort(argv); m != "model-b" || e != "high" {
		t.Errorf("model, effort = %q, %q", m, e)
	}
	if m, e := codexArgsModelEffort([]string{"exec", "--json", "-"}); m != "" || e != "" {
		t.Errorf("no pins gave %q, %q", m, e)
	}
}

// The launcher runs the hook once codex has exited and before its exit
// record, with the run's own segment offset, run id and instance, and the
// log on stdin.
func TestCodexLauncherRunsUsageHookBeforeExitRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fakecodex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexec '"+fake+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	hook := filepath.Join(out, "agentmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + out + "/args'\nenv | grep '^AGENTMUX_USAGE_' > '" + out + "/env'\ncat > '" + out + "/stdin'\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := usageHookBinary
	usageHookBinary = func() string { return hook }
	defer func() { usageHookBinary = old }()

	logPath := filepath.Join(t.TempDir(), "run.jsonl")
	prior := `{"type":"turn.completed","usage":{"input_tokens":1}}` + "\n"
	if err := os.WriteFile(logPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	argv, _ := CodexRunArgs(CodexRunOptions{Model: "model-c", Effort: "low"}, "")
	proc, runID, err := startCodexProcessDetached(context.Background(), "task-p-p-2", argv, "hi", t.TempDir(), logPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Release()
	if !waitForLog(logPath, `"agentmux.exit"`, 15*time.Second) {
		t.Fatalf("no exit record: %q", readLog(logPath))
	}
	args, _ := os.ReadFile(filepath.Join(out, "args"))
	want := strings.Join([]string{"usage-hook", "codex", "--instance", "task-p-p-2", "--run", runID, "--offset", strconv.Itoa(len(prior))}, "\n") + "\n"
	if string(args) != want {
		t.Errorf("hook args = %q, want %q", args, want)
	}
	stdin, _ := os.ReadFile(filepath.Join(out, "stdin"))
	if !strings.HasPrefix(string(stdin), prior) || !strings.Contains(string(stdin), `"turn.completed"`) || strings.Contains(string(stdin), "agentmux.exit") {
		t.Errorf("hook stdin = %q, want the log up to before the exit record", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(out, "env"))
	if !strings.Contains(string(env), "AGENTMUX_USAGE_MODEL=model-c") || !strings.Contains(string(env), "AGENTMUX_USAGE_EFFORT=low") {
		t.Errorf("hook env = %q", env)
	}
}

// Both hooks end in one event each in the run user's file; a Claude hook
// firing twice writes one.
func TestUsageHooksWriteEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "no-codex"))
	events := filepath.Join(home, "turns.jsonl")
	t.Setenv("AGENTMUX_USAGE_EVENTS", events)
	t.Setenv(usageHookModelEnv, "model-d")
	t.Setenv(usageHookEffortEnv, "")

	transcript := filepath.Join(home, "s.jsonl")
	os.WriteFile(transcript, []byte(`{"type":"user","message":{"role":"user","content":"go"}}`+"\n"+
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"model-e","usage":{"input_tokens":2,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}}}`+"\n"), 0o600)
	in := `{"session_id":"s1","transcript_path":"` + transcript + `","hook_event_name":"Stop"}`
	UsageHookClaude("task-p-p-3", strings.NewReader(in))
	UsageHookClaude("task-p-p-3", strings.NewReader(in))

	logPath := filepath.Join(home, "run.jsonl")
	os.WriteFile(logPath, []byte(`{"type":"thread.started","thread_id":"t9"}`+"\n"+
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":5}}`+"\n"), 0o600)
	f, _ := os.Open(logPath)
	defer f.Close()
	UsageHookCodex("task-p-p-4", "r1", 0, f)

	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatalf("no events (errors: %s)", readLog(strings.TrimSuffix(events, ".jsonl")+".errors"))
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("events = %s", data)
	}
	for i, want := range []string{
		`"turn_id":"claude:s1:a1","runner":"claude-code","instance":"task-p-p-3","task":"P-3","model":"model-e","session":"s1","requests":1,"input_tokens":2,"output_tokens":3,"cache_read_tokens":4,"cache_write_tokens":5`,
		`"turn_id":"codex:t9:r1","runner":"codex","instance":"task-p-p-4","task":"P-4","model":"model-d","session":"t9","requests":0,"input_tokens":6,"output_tokens":5,"cache_read_tokens":4,"cache_write_tokens":0`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("event %d = %s\nwant it to contain %s", i, lines[i], want)
		}
	}
}
