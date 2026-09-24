package threadwatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeTestOffsets is a minimal in-memory OffsetStore for collector tests.
type claudeTestOffsets struct{ m map[string]int64 }

func newClaudeTestOffsets() *claudeTestOffsets { return &claudeTestOffsets{m: map[string]int64{}} }

func (o *claudeTestOffsets) Get(key string) (int64, bool) { v, ok := o.m[key]; return v, ok }
func (o *claudeTestOffsets) Set(key string, value int64)  { o.m[key] = value }

// claudeTestInstance lays out <home>/.claude/projects/<slug>/ for workdir
// and returns the Instance plus that project directory.
func claudeTestInstance(t *testing.T, workdir string) (Instance, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", claudeProjectSlug(workdir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return Instance{Name: "inst1", Agent: "claude-code", Workdir: workdir, Home: home}, dir
}

func claudeCopyFixture(t *testing.T, fixture, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeProjectSlug(t *testing.T) {
	if got, want := claudeProjectSlug("/home/u/apps/x"), "-home-u-apps-x"; got != want {
		t.Errorf("claudeProjectSlug = %q, want %q", got, want)
	}
	if got, want := claudeProjectSlug("/home/u/my.app"), "-home-u-my-app"; got != want {
		t.Errorf("claudeProjectSlug = %q, want %q", got, want)
	}
}

func TestClaudeCollectorMapsRecords(t *testing.T) {
	inst, dir := claudeTestInstance(t, "/proj/one")
	claudeCopyFixture(t, "session.jsonl", filepath.Join(dir, "a.jsonl"))

	c := &ClaudeCollector{Backfill: true}
	offsets := newClaudeTestOffsets()
	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{
		KindUserMessage,
		KindActivity,
		KindToolError,
		KindAPIError,
		KindActivity, KindAssistantMsg,
		KindCompaction,
		KindTurnEnd,
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}

	for _, e := range events {
		if e.Instance != "inst1" || e.Agent != "claude-code" || e.Thread != "sess-1" {
			t.Errorf("event %+v has wrong instance/agent/thread", e)
		}
	}

	// user plain text
	if got := events[0].Excerpt; got != "Please add a health check endpoint." {
		t.Errorf("user message excerpt = %q", got)
	}

	// tool_result error, attributed to the tool_use block's name
	te := events[2]
	if te.Tool != "Write" {
		t.Errorf("tool error Tool = %q, want Write", te.Tool)
	}
	if !strings.Contains(te.Excerpt, "Permission denied") {
		t.Errorf("tool error excerpt = %q", te.Excerpt)
	}

	// isApiErrorMessage
	if got := events[3].Excerpt; got != "rate limit exceeded, retrying" {
		t.Errorf("api error excerpt = %q", got)
	}

	// final assistant text
	if got := events[5].Excerpt; got != "I'll retry that." {
		t.Errorf("assistant msg excerpt = %q", got)
	}

	// turn_end: duration and deduped usage (msg_1's usage counted once,
	// not twice, and the sidechain message's usage is not folded in).
	end := events[7]
	if end.Duration != 45231*time.Millisecond {
		t.Errorf("turn_end duration = %v", end.Duration)
	}
	wantUsage := Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheWriteTokens: 3}
	if end.Tokens != wantUsage {
		t.Errorf("turn_end tokens = %+v, want %+v", end.Tokens, wantUsage)
	}
}

func TestClaudeCollectorRedactsExcerpts(t *testing.T) {
	inst, dir := claudeTestInstance(t, "/proj/two")
	claudeCopyFixture(t, "redact.jsonl", filepath.Join(dir, "a.jsonl"))

	c := &ClaudeCollector{Backfill: true}
	events, err := c.Poll(context.Background(), inst, newClaudeTestOffsets())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if strings.Contains(events[0].Excerpt, "abc123def456") {
		t.Errorf("excerpt leaked secret: %q", events[0].Excerpt)
	}
	if !strings.Contains(events[0].Excerpt, "[redacted]") {
		t.Errorf("excerpt not redacted: %q", events[0].Excerpt)
	}
}

func TestClaudeCollectorBackfillFalseStartsAtEOF(t *testing.T) {
	inst, dir := claudeTestInstance(t, "/proj/three")
	path := filepath.Join(dir, "a.jsonl")
	claudeCopyFixture(t, "tail.jsonl", path)

	c := &ClaudeCollector{} // Backfill defaults false
	offsets := newClaudeTestOffsets()

	// First poll on a never-before-seen file: no events, and the offset
	// lands at end-of-file rather than replaying history.
	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("first poll (Backfill=false) got %d events, want 0", len(events))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if off, _ := offsets.Get("claude:" + path); off != info.Size() {
		t.Fatalf("offset = %d, want file size %d", off, info.Size())
	}

	// A subsequent line written after that point is picked up normally.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"user","timestamp":"2026-01-01T00:01:00Z","sessionId":"sess-3","message":{"role":"user","content":"second message"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err = c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Excerpt != "second message" {
		t.Fatalf("second poll events = %+v", events)
	}
}

func TestClaudeCollectorPartialLineResumption(t *testing.T) {
	inst, dir := claudeTestInstance(t, "/proj/four")
	path := filepath.Join(dir, "a.jsonl")
	claudeCopyFixture(t, "tail.jsonl", path)

	c := &ClaudeCollector{Backfill: true}
	offsets := newClaudeTestOffsets()

	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	offAfterFirst, _ := offsets.Get("claude:" + path)

	// Append a line with no trailing newline yet: still being written.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	partial := `{"type":"user","timestamp":"2026-01-01T00:02:00Z","sessionId":"sess-3","message":{"role":"user","content":"incomple`
	if _, err := f.WriteString(partial); err != nil {
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
	if off, _ := offsets.Get("claude:" + path); off != offAfterFirst {
		t.Fatalf("offset advanced past partial line: %d != %d", off, offAfterFirst)
	}

	// Complete the line: it should now be consumed.
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`te text"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	events, err = c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Excerpt != "incomplete text" {
		t.Fatalf("completed-line poll events = %+v", events)
	}
}

func TestClaudeCollectorTruncationRestartsAtZero(t *testing.T) {
	inst, dir := claudeTestInstance(t, "/proj/five")
	path := filepath.Join(dir, "a.jsonl")
	claudeCopyFixture(t, "session.jsonl", path)

	c := &ClaudeCollector{Backfill: true}
	offsets := newClaudeTestOffsets()
	if _, err := c.Poll(context.Background(), inst, offsets); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if off, _ := offsets.Get("claude:" + path); off == 0 {
		t.Fatal("expected non-zero offset after first poll")
	}

	// Truncate to a short file: the collector must not treat this as
	// "nothing new" (offset > size) and must restart from 0.
	claudeCopyFixture(t, "tail.jsonl", path)

	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Excerpt != "first message" {
		t.Fatalf("post-truncation events = %+v", events)
	}
}
