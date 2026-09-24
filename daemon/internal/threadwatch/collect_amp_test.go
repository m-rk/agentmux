package threadwatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ampTestOffsets is a minimal in-memory OffsetStore for collector tests.
type ampTestOffsets struct{ m map[string]int64 }

func newAmpTestOffsets() *ampTestOffsets { return &ampTestOffsets{m: map[string]int64{}} }

func (o *ampTestOffsets) Get(key string) (int64, bool) { v, ok := o.m[key]; return v, ok }
func (o *ampTestOffsets) Set(key string, value int64)  { o.m[key] = value }

func ampTestInstance(t *testing.T) (Instance, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".cache", "amp", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return Instance{Name: "inst1", Agent: "amp", Home: home}, filepath.Join(dir, "no-tui.log")
}

func ampCopyFixture(t *testing.T, fixture, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "amp", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAmpCollectorMapsRecords(t *testing.T) {
	inst, path := ampTestInstance(t)
	ampCopyFixture(t, "session.jsonl", path)

	var c AmpCollector
	events, err := c.Poll(context.Background(), inst, newAmpTestOffsets())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{
		KindAPIError,  // failed to call tool
		KindAuthError, // session expired
		KindAuthError, // 401
		KindActivity,  // agent state: working
		KindActivity,  // executing tool: Read
		KindTurnEnd,   // agent state: idle
		KindAPIError,  // upload failed (redacted)
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}

	for _, e := range events {
		if e.Instance != "inst1" || e.Agent != "amp" {
			t.Errorf("event %+v has wrong instance/agent", e)
		}
	}

	if events[4].Tool != "Read" {
		t.Errorf("tool = %q, want Read", events[4].Tool)
	}
	if events[5].Duration != 2*time.Second {
		t.Errorf("turn duration = %v, want 2s", events[5].Duration)
	}
	if got := events[0].Excerpt; got != "failed to call tool: ECONNRESET" {
		t.Errorf("api error excerpt = %q", got)
	}
	if e := events[len(events)-1]; strings.Contains(e.Excerpt, "abcdefghijklmnop") || !strings.Contains(e.Excerpt, "[redacted]") {
		t.Errorf("excerpt not redacted: %q", e.Excerpt)
	}
}

func TestAmpCollectorPartialLineResumption(t *testing.T) {
	inst, path := ampTestInstance(t)
	if err := os.WriteFile(path, []byte(`{"@timestamp":"2026-01-01T00:00:00Z","level":"INFO","message":"x executing tool: Read","threadId":"t1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var c AmpCollector
	offsets := newAmpTestOffsets()
	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	offAfterFirst, _ := offsets.Get("amp:" + path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"@timestamp":"2026-01-01T00:00:01Z","level":"INFO","message":"x executing tool: Ed`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err = c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("partial-line poll got %d events, want 0", len(events))
	}
	if off, _ := offsets.Get("amp:" + path); off != offAfterFirst {
		t.Fatalf("offset advanced past partial line: %d != %d", off, offAfterFirst)
	}
}

func TestAmpCollectorTruncationRestartsAtZero(t *testing.T) {
	inst, path := ampTestInstance(t)
	ampCopyFixture(t, "session.jsonl", path)

	var c AmpCollector
	offsets := newAmpTestOffsets()
	if _, err := c.Poll(context.Background(), inst, offsets); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	short := `{"@timestamp":"2026-01-01T01:00:00Z","level":"INFO","message":"x executing tool: Read","threadId":"t2"}` + "\n"
	if err := os.WriteFile(path, []byte(short), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Kind != KindActivity || events[0].Thread != "t2" {
		t.Fatalf("post-truncation events = %+v", events)
	}
}

func TestAmpCollectorRotationByInode(t *testing.T) {
	inst, path := ampTestInstance(t)
	if err := os.WriteFile(path, []byte(`{"@timestamp":"2026-01-01T00:00:00Z","level":"INFO","message":"x executing tool: Read","threadId":"old"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var c AmpCollector
	offsets := newAmpTestOffsets()
	if _, err := c.Poll(context.Background(), inst, offsets); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Simulate log rotation: unlink and recreate the file (new inode),
	// with content shorter than the old offset would allow re-reading
	// naturally, to make sure inode change - not just size - triggers
	// the restart.
	replacement := strings.Repeat(" ", 200) + `
{"@timestamp":"2026-01-01T01:00:00Z","level":"INFO","message":"x executing tool: Read","threadId":"new"}
`
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Thread != "new" {
		t.Fatalf("post-rotation events = %+v, want a single event for thread new", events)
	}
}

func TestAmpCollectorIgnoresReconnectChatterAndInfoNoise(t *testing.T) {
	inst, path := ampTestInstance(t)
	lines := []string{
		`{"@timestamp":"2026-01-01T00:00:00Z","level":"INFO","message":"NPM version comparison: current 1.2.3 latest 1.2.3"}`,
		`{"@timestamp":"2026-01-01T00:00:01Z","level":"WARN","message":"reconnecting to thread after socket drop","threadId":"t1"}`,
		`{"@timestamp":"2026-01-01T00:00:02Z","level":"INFO","message":"runner registered"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var c AmpCollector
	events, err := c.Poll(context.Background(), inst, newAmpTestOffsets())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0: %+v", len(events), events)
	}
}

func TestAmpCollectorAgentStateTransitions(t *testing.T) {
	inst, path := ampTestInstance(t)
	state := func(sec int, thread, subtype string) string {
		return fmt.Sprintf(`{"@timestamp":"2026-01-01T00:00:%02dZ","level":"INFO","message":"[observer] onAgentState","type":"agent_state","subtype":%q,"threadId":%q}`, sec, subtype, thread)
	}
	lines := []string{
		state(0, "t1", "idle"),       // turn already over when first seen: nothing
		state(1, "t1", "working"),    // turn starts
		state(2, "t1", "streaming"),  // progress
		state(3, "t1", "streaming"),  // repeat: dropped
		state(4, "t1", "compacting"), // compaction
		state(9, "t1", "idle"),       // turn ends after 8s
		state(10, "t1", "idle"),      // repeat: dropped
		state(11, "t2", "tool_use"),  // other thread, joined mid-turn
		state(15, "t2", "idle"),      // ends 4s after first seen
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c AmpCollector
	events, err := c.Poll(context.Background(), inst, newAmpTestOffsets())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s/%s/%v", e.Thread, e.Kind, e.Duration))
	}
	want := []string{
		"t1/activity/0s", "t1/activity/0s", "t1/compaction/0s", "t1/turn_end/8s",
		"t2/activity/0s", "t2/turn_end/4s",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
