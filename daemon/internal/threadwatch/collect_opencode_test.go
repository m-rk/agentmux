package threadwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- in-memory OffsetStore for tests ------------------------------------

type memOffsets map[string]int64

func (m memOffsets) Get(key string) (int64, bool) {
	v, ok := m[key]
	return v, ok
}

func (m memOffsets) Set(key string, value int64) {
	m[key] = value
}

// --- pure unit tests of row -> Event mapping ----------------------------

func msgJSON(t *testing.T, role string, created, completed int64, tokens, cost, errBlock string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, `{"role":%q,"time":{"created":%d`, role, created)
	if completed > 0 {
		fmt.Fprintf(&b, `,"completed":%d`, completed)
	}
	b.WriteString("}")
	if tokens != "" {
		fmt.Fprintf(&b, `,"tokens":%s`, tokens)
	}
	if cost != "" {
		fmt.Fprintf(&b, `,"cost":%s`, cost)
	}
	if errBlock != "" {
		fmt.Fprintf(&b, `,"error":%s`, errBlock)
	}
	b.WriteString("}")
	return b.String()
}

func partJSON(typ, tool, text, status, stateErr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":%q`, typ)
	if tool != "" {
		fmt.Fprintf(&b, `,"tool":%q`, tool)
	}
	if text != "" {
		fmt.Fprintf(&b, `,"text":%q`, text)
	}
	if status != "" {
		fmt.Fprintf(&b, `,"state":{"status":%q`, status)
		if stateErr != "" {
			fmt.Fprintf(&b, `,"error":%q`, stateErr)
		}
		b.WriteString("}")
	}
	b.WriteString("}")
	return b.String()
}

func TestBuildOpencodeEventsTurnEndWithAssistantExcerpt(t *testing.T) {
	inst := Instance{Name: "probe", Workdir: "/work"}
	msgRows := []opencodeMessageRow{
		{
			ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1500,
			Data: msgJSON(t, "assistant", 1000, 1500, `{"input":10,"output":20,"cache":{"write":1,"read":2}}`, "0.0012", ""),
		},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1100, TimeUpdated: 1100, Data: partJSON("text", "", "hello", "", "")},
		{ID: "p2", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1400, TimeUpdated: 1400, Data: partJSON("text", "", "final answer", "", "")},
	}

	events, newOffset := buildOpencodeEvents(inst, msgRows, partRows)

	if newOffset != 1500 {
		t.Fatalf("newOffset = %d, want 1500", newOffset)
	}

	var turnEnd, assistantMsg, activity int
	for _, ev := range events {
		if ev.Instance != "probe" || ev.Agent != "opencode" || ev.Thread != "ses1" {
			t.Errorf("bad event envelope: %+v", ev)
		}
		switch ev.Kind {
		case KindTurnEnd:
			turnEnd++
			if ev.Duration != 500*time.Millisecond {
				t.Errorf("Duration = %v, want 500ms", ev.Duration)
			}
			if ev.Tokens.InputTokens != 10 || ev.Tokens.OutputTokens != 20 || ev.Tokens.CacheReadTokens != 2 || ev.Tokens.CacheWriteTokens != 1 {
				t.Errorf("Tokens = %+v", ev.Tokens)
			}
			if ev.Tokens.CostUSD != 0.0012 {
				t.Errorf("CostUSD = %v", ev.Tokens.CostUSD)
			}
		case KindAssistantMsg:
			assistantMsg++
			if ev.Excerpt != "final answer" {
				t.Errorf("assistant excerpt = %q, want %q", ev.Excerpt, "final answer")
			}
		case KindActivity:
			activity++
			if ev.Excerpt != "hello" {
				t.Errorf("activity excerpt = %q, want %q (interim text)", ev.Excerpt, "hello")
			}
		default:
			t.Errorf("unexpected kind %q", ev.Kind)
		}
	}
	if turnEnd != 1 || assistantMsg != 1 || activity != 1 {
		t.Fatalf("counts: turnEnd=%d assistantMsg=%d activity=%d", turnEnd, assistantMsg, activity)
	}
}

func TestBuildOpencodeEventsAPIError(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{
			ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1200,
			Data: msgJSON(t, "assistant", 1000, 1200, "", "", `{"name":"APIError","data":{"message":"Bad Gateway"}}`),
		},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, nil)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if events[0].Kind != KindAPIError {
		t.Fatalf("Kind = %q, want api_error", events[0].Kind)
	}
	if events[0].Excerpt != "Bad Gateway" {
		t.Fatalf("Excerpt = %q", events[0].Excerpt)
	}
}

func TestBuildOpencodeEventsAuthError(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{
			ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1200,
			Data: msgJSON(t, "assistant", 1000, 1200, "", "", `{"name":"APIError","data":{"message":"Invalid token (request id: abc)"}}`),
		},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, nil)
	if len(events) != 1 || events[0].Kind != KindAuthError {
		t.Fatalf("events = %+v, want single auth_error", events)
	}
}

func TestBuildOpencodeEventsUserMessage(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: msgJSON(t, "user", 1000, 0, "", "", "")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("text", "", "please do X", "", "")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, partRows)
	if len(events) != 1 || events[0].Kind != KindUserMessage || events[0].Excerpt != "please do X" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildOpencodeEventsToolError(t *testing.T) {
	inst := Instance{Name: "probe"}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("tool", "bash", "", "error", "exit status 1")},
	}
	events, _ := buildOpencodeEvents(inst, nil, partRows)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != KindToolError || ev.Tool != "bash" || ev.Excerpt != "exit status 1" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestBuildOpencodeEventsToolCompletedIsActivity(t *testing.T) {
	inst := Instance{Name: "probe"}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("tool", "read", "", "completed", "")},
	}
	events, _ := buildOpencodeEvents(inst, nil, partRows)
	if len(events) != 1 || events[0].Kind != KindActivity || events[0].Tool != "read" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildOpencodeEventsCompaction(t *testing.T) {
	inst := Instance{Name: "probe"}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: `{"type":"compaction"}`},
	}
	events, _ := buildOpencodeEvents(inst, nil, partRows)
	if len(events) != 1 || events[0].Kind != KindCompaction {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildOpencodeEventsInProgressAssistantIsActivity(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1100, Data: msgJSON(t, "assistant", 1000, 0, "", "", "")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1100, TimeUpdated: 1100, Data: partJSON("text", "", "partial...", "", "")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, partRows)
	if len(events) != 1 || events[0].Kind != KindActivity || events[0].Excerpt != "partial..." {
		t.Fatalf("events = %+v, want a single activity event for interim text", events)
	}
}

func TestBuildOpencodeEventsExcerptIsRedacted(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: msgJSON(t, "user", 1000, 0, "", "", "")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("text", "", "here is sk-ant-abcdef123456789012 ok", "", "")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, partRows)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if strings.Contains(events[0].Excerpt, "sk-ant-abcdef123456789012") {
		t.Fatalf("Excerpt not redacted: %q", events[0].Excerpt)
	}
}

// --- OffsetStore/backfill semantics (no sqlite3 needed) ------------------

func TestOpencodeCollectorMissingDB(t *testing.T) {
	c := &OpencodeCollector{}
	inst := Instance{Name: "probe", Home: t.TempDir(), Workdir: "/work"}
	events, err := c.Poll(context.Background(), inst, memOffsets{})
	if !errors.Is(err, ErrSourceMissing) {
		t.Fatalf("err = %v, want ErrSourceMissing", err)
	}
	if events != nil {
		t.Fatalf("events = %v, want nil", events)
	}
}

func TestOpencodeCollectorMissingSqlite3Binary(t *testing.T) {
	home := t.TempDir()
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "opencode.db"), []byte("not a real db, existence is enough"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &OpencodeCollector{SqlitePath: filepath.Join(t.TempDir(), "no-such-sqlite3-binary")}
	inst := Instance{Name: "probe", Home: home, Workdir: "/work"}
	events, err := c.Poll(context.Background(), inst, memOffsets{})
	if !errors.Is(err, ErrSourceMissing) {
		t.Fatalf("err = %v, want ErrSourceMissing", err)
	}
	if events != nil {
		t.Fatalf("events = %v, want nil", events)
	}
}

// --- end-to-end against a real sqlite3 CLI and fixture DB ---------------

func requireSqlite3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed, skipping")
	}
}

// newFixtureDB builds a throwaway opencode-shaped SQLite database using the
// real sqlite3 CLI, mirroring the subset of the on-disk schema this
// collector reads (session, project, message, part).
func newFixtureDB(t *testing.T, dbPath string) {
	t.Helper()
	schema := `
CREATE TABLE project (
  id text PRIMARY KEY,
  worktree text NOT NULL
);
CREATE TABLE session (
  id text PRIMARY KEY,
  project_id text,
  directory text NOT NULL,
  time_created integer NOT NULL,
  time_updated integer NOT NULL
);
CREATE TABLE message (
  id text PRIMARY KEY,
  session_id text NOT NULL,
  time_created integer NOT NULL,
  time_updated integer NOT NULL,
  data text NOT NULL
);
CREATE TABLE part (
  id text PRIMARY KEY,
  message_id text NOT NULL,
  session_id text NOT NULL,
  time_created integer NOT NULL,
  time_updated integer NOT NULL,
  data text NOT NULL
);
INSERT INTO project (id, worktree) VALUES ('proj1', '/work');
INSERT INTO session (id, project_id, directory, time_created, time_updated) VALUES ('ses1', 'proj1', '/work', 1000, 1000);
`
	cmd := exec.Command("sqlite3", dbPath, schema)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 create fixture: %v: %s", err, out)
	}
}

func fixtureExec(t *testing.T, dbPath, sql string) {
	t.Helper()
	cmd := exec.Command("sqlite3", dbPath, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 exec: %v: %s", err, out)
	}
}

func TestOpencodeCollectorEndToEnd(t *testing.T) {
	requireSqlite3(t)

	home := t.TempDir()
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "opencode.db")
	newFixtureDB(t, dbPath)

	// Pre-existing history, written before threadwatch ever polls.
	fixtureExec(t, dbPath, `
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_old', 'ses1', 100, 200, '{"role":"user","time":{"created":100}}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p_old', 'm_old', 'ses1', 100, 200, '{"type":"text","text":"old message, should not replay"}');
`)

	c := &OpencodeCollector{}
	inst := Instance{Name: "probe", Home: home, Workdir: "/work"}
	offsets := memOffsets{}

	// First poll with no stored offset and Backfill=false: no replay.
	events, err := c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("first Poll events = %+v, want none (no replay)", events)
	}
	key := "opencode:" + dbPath + ":/work"
	if _, ok := offsets.Get(key); !ok {
		t.Fatalf("offset not stored after first poll")
	}

	// New activity after the first poll: a full user -> assistant turn.
	fixtureExec(t, dbPath, `
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_user', 'ses1', 300, 300, '{"role":"user","time":{"created":300}}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p_user', 'm_user', 'ses1', 300, 300, '{"type":"text","text":"do the thing"}');
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_asst', 'ses1', 310, 400, '{"role":"assistant","time":{"created":310,"completed":400},"tokens":{"input":5,"output":7,"cache":{"write":0,"read":0}},"cost":0.001}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p_asst', 'm_asst', 'ses1', 390, 390, '{"type":"text","text":"done"}');
`)

	events, err = c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	var gotUser, gotTurnEnd, gotAssistant bool
	for _, ev := range events {
		if ev.Instance != "probe" || ev.Agent != "opencode" || ev.Thread != "ses1" {
			t.Errorf("bad envelope: %+v", ev)
		}
		switch ev.Kind {
		case KindUserMessage:
			gotUser = true
			if ev.Excerpt != "do the thing" {
				t.Errorf("user excerpt = %q", ev.Excerpt)
			}
		case KindTurnEnd:
			gotTurnEnd = true
			if ev.Duration != 90*time.Millisecond {
				t.Errorf("Duration = %v, want 90ms", ev.Duration)
			}
		case KindAssistantMsg:
			gotAssistant = true
			if ev.Excerpt != "done" {
				t.Errorf("assistant excerpt = %q", ev.Excerpt)
			}
		}
	}
	if !gotUser || !gotTurnEnd || !gotAssistant {
		t.Fatalf("missing expected kinds in %+v", events)
	}
	for _, ev := range events {
		if ev.Excerpt == "old message, should not replay" {
			t.Fatalf("pre-existing row replayed: %+v", events)
		}
	}

	// Third poll with nothing new: no events, no error.
	events, err = c.Poll(context.Background(), inst, offsets)
	if err != nil {
		t.Fatalf("third Poll: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("third Poll events = %+v, want none", events)
	}
}

func TestOpencodeCollectorBackfill(t *testing.T) {
	requireSqlite3(t)

	home := t.TempDir()
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "opencode.db")
	newFixtureDB(t, dbPath)
	fixtureExec(t, dbPath, `
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_old', 'ses1', 100, 200, '{"role":"user","time":{"created":100}}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p_old', 'm_old', 'ses1', 100, 200, '{"type":"text","text":"replay me"}');
`)

	c := &OpencodeCollector{Backfill: true}
	inst := Instance{Name: "probe", Home: home, Workdir: "/work"}
	events, err := c.Poll(context.Background(), inst, memOffsets{})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Excerpt != "replay me" {
		t.Fatalf("events = %+v, want the backfilled user message", events)
	}
}

func TestOpencodeCollectorNoMatchingSessions(t *testing.T) {
	requireSqlite3(t)

	home := t.TempDir()
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "opencode.db")
	newFixtureDB(t, dbPath)

	c := &OpencodeCollector{}
	inst := Instance{Name: "probe", Home: home, Workdir: "/some/other/dir"}
	events, err := c.Poll(context.Background(), inst, memOffsets{})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if events != nil {
		t.Fatalf("events = %v, want nil (no sessions match workdir)", events)
	}
}
