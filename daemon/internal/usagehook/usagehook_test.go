package usagehook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexLog = `{"type":"agentmux.start","run":"r1","pid":1,"at":"2026-11-23T10:00:00Z"}
{"type":"thread.started","thread_id":"t-1"}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"cache_write_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":10}}
{"type":"agentmux.exit","run":"r1","code":0,"at":"2026-11-23T10:01:00Z"}
`

const codexRun2 = `{"type":"agentmux.start","run":"r2","pid":2,"at":"2026-11-23T11:00:00Z"}
{"type":"thread.started","thread_id":"t-1"}
{"type":"item.completed","item":{"type":"command_execution","text":"` + "%s" + `"}}
{"type":"turn.completed","usage":{"input_tokens":1500,"cached_input_tokens":1100,"cache_write_input_tokens":20,"output_tokens":80,"reasoning_output_tokens":15}}
`

func readRun(t *testing.T, log string, start int64) CodexRun {
	t.Helper()
	r, err := ReadCodexRun(strings.NewReader(log), int64(len(log)), start)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A run's usage is its own: codex reports the thread's running total, so a
// resumed thread's second run is that total minus the first run's, with
// cached input taken out of input. A huge output line in between is skipped.
func TestReadCodexRunIsTheRunsOwnDelta(t *testing.T) {
	first := readRun(t, codexLog, 0)
	want := Tokens{Input: 200, CacheRead: 800, Output: 50, Reasoning: 10}
	if !first.Completed || first.Thread != "t-1" || first.Tokens != want {
		t.Errorf("first run = %+v, want completed t-1 %+v", first, want)
	}
	big := strings.Repeat("x", maxCodexLine+10)
	log := codexLog + strings.Replace(codexRun2, "%s", big, 1)
	second := readRun(t, log, int64(len(codexLog)))
	want = Tokens{Input: 200, CacheRead: 300, CacheWrite: 20, Output: 30, Reasoning: 5}
	if !second.Completed || second.Tokens != want {
		t.Errorf("second run = %+v, want %+v", second, want)
	}
}

// A failed run (no turn.completed) reports nothing rather than a guess.
func TestReadCodexRunFailedHasNoUsage(t *testing.T) {
	log := codexLog + `{"type":"agentmux.start","run":"r2"}` + "\n" + `{"type":"turn.failed","error":{"message":"boom"}}` + "\n"
	if r := readRun(t, log, int64(len(codexLog))); r.Completed || !r.Tokens.Zero() {
		t.Errorf("failed run = %+v, want no usage", r)
	}
}

func jsonLine(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func prompt(t *testing.T, text string) string {
	return jsonLine(t, map[string]any{"type": "user", "uuid": "u-" + text, "message": map[string]any{"role": "user", "content": text}})
}

func toolResult(t *testing.T) string {
	return jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}})
}

func reply(t *testing.T, uuid, id string, in, out, cr, cw int64) string {
	return jsonLine(t, map[string]any{"type": "assistant", "uuid": uuid, "message": map[string]any{
		"id": id, "model": "model-a",
		"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": cr, "cache_creation_input_tokens": cw},
	}})
}

// The Claude reader counts each API message once, keeps a cursor so a hook
// that fires twice counts nothing new, leaves a half-written line for next
// time, and on first sight of a transcript counts only its last turn.
func TestReadClaudeTurnCountsOnlyNewMessagesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	cursors := filepath.Join(dir, "cursors")
	old := prompt(t, "earlier") + reply(t, "a0", "m0", 999, 999, 999, 999)
	turn1 := prompt(t, "now") +
		reply(t, "a1", "m1", 3, 10, 100, 20) + reply(t, "a2", "m1", 3, 40, 100, 20) + // one message streamed over two lines
		toolResult(t) + reply(t, "a3", "m2", 1, 5, 200, 0)
	if err := os.WriteFile(path, []byte(old+turn1), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadClaudeTurn(path, cursors)
	if err != nil {
		t.Fatal(err)
	}
	want := Tokens{Requests: 2, Input: 4, Output: 45, CacheRead: 300, CacheWrite: 20}
	if got.Tokens != want || got.LastUUID != "a3" || got.Model != "model-a" {
		t.Fatalf("first turn = %+v, want %+v from a3", got, want)
	}
	if again, _ := ReadClaudeTurn(path, cursors); !again.Tokens.Zero() {
		t.Errorf("second firing counted %+v", again.Tokens)
	}

	// Next turn, its last line not yet finished.
	next := prompt(t, "more") + reply(t, "a4", "m3", 2, 7, 50, 0)
	partial := strings.TrimSuffix(reply(t, "a5", "m4", 1, 1, 1, 1), "\n")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(next + partial)
	f.Close()
	got, _ = ReadClaudeTurn(path, cursors)
	if want := (Tokens{Requests: 1, Input: 2, Output: 7, CacheRead: 50}); got.Tokens != want {
		t.Errorf("second turn = %+v, want %+v", got.Tokens, want)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n")
	f.Close()
	got, _ = ReadClaudeTurn(path, cursors)
	if want := (Tokens{Requests: 1, Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}); got.Tokens != want {
		t.Errorf("finished line = %+v, want %+v", got.Tokens, want)
	}
}

// Events carry forgentic's identity from the instance's OTEL attributes and
// are whole lines, one write each.
func TestAppendWritesVersionedLineWithIdentity(t *testing.T) {
	fields := map[string]string{
		"AGENTMUX_ENV_OTEL_RESOURCE_ATTRIBUTES": "task_id=MERG-415,run_id=r9,project=m-rk/mergentic,offering=x",
		"AGENTMUX_PROJECT":                      "github.com/m-rk/other",
	}
	id := IdentityFrom("task-mergentic-merg-415", fields, "model-a", "medium")
	if id.Task != "MERG-415" || id.Run != "r9" || id.Project != "m-rk/mergentic" {
		t.Errorf("identity = %+v", id)
	}
	fallback := IdentityFrom("task-uptime-kuma-kuma-2", map[string]string{"AGENTMUX_PROJECT": "github.com/m-rk/uptime-kuma"}, "", "")
	if fallback.Task != "KUMA-2" || fallback.Project != "m-rk/uptime-kuma" {
		t.Errorf("fallback identity = %+v", fallback)
	}
	if IdentityFrom("forgentic-orchestrator", nil, "", "").Task != "" {
		t.Error("a non-task instance got a task")
	}

	path := filepath.Join(t.TempDir(), "m", "turns.jsonl")
	e := id.Event("codex", "codex:t:r", "t", Tokens{Input: 1, Output: 2, CacheRead: 3})
	if err := Append(path, e); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, e); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %s", len(lines), data)
	}
	var got map[string]any
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{"v": 1.0, "turn_id": "codex:t:r", "runner": "codex", "task": "MERG-415", "run": "r9", "model": "model-a", "effort": "medium", "input_tokens": 1.0, "output_tokens": 2.0, "cache_read_tokens": 3.0, "cache_write_tokens": 0.0} {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if err := Append(path, Event{Runner: "codex"}); err == nil {
		t.Error("event without turn id accepted")
	}
}
