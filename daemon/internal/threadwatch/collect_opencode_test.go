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

// msgJSON builds a message.data JSON blob. finish is opencode's per-step
// finish reason ("tool-calls" for an intermediate step, "stop"/"length"/
// "unknown"/"" for whatever ends a turn); pass "" to omit the field
// entirely, matching real rows where it's simply absent.
func msgJSON(role string, created, completed int64, finish string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"role":%q,"time":{"created":%d`, role, created)
	if completed > 0 {
		fmt.Fprintf(&b, `,"completed":%d`, completed)
	}
	b.WriteString("}")
	if finish != "" {
		fmt.Fprintf(&b, `,"finish":%q`, finish)
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
	// The terminal message row is deliberately included in msgRows too
	// (as queryMessages would return it alongside queryTurns): it must be
	// skipped there and driven entirely by the turnRows aggregate, or it
	// would double up (this is a regression test for the "one turn_end per
	// completed assistant message" bug).
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1500, Data: msgJSON("assistant", 1000, 1500, "stop")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1100, TimeUpdated: 1100, Data: partJSON("text", "", "hello", "", "")},
		{ID: "p2", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1400, TimeUpdated: 1400, Data: partJSON("text", "", "final answer", "", "")},
	}
	turnRows := []opencodeTurnRow{
		{
			SessionID: "ses1", TerminalID: "msg1", TerminalTimeUpdated: 1500,
			Completed: 1500, StartTime: 1000,
			SumIn: 10, SumOut: 20, SumCacheWrite: 1, SumCacheRead: 2, SumCost: 0.0012,
		},
	}

	events, newOffset := buildOpencodeEvents(inst, msgRows, partRows, turnRows)

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
		t.Fatalf("counts: turnEnd=%d assistantMsg=%d activity=%d (want exactly one turn_end, not one per step)", turnEnd, assistantMsg, activity)
	}
}

// TestBuildOpencodeEventsMultiStepTurn is the core regression case: opencode
// writes one assistant message per tool-call round (finish=="tool-calls")
// before the message that actually ends the turn. This must produce exactly
// one KindTurnEnd (not one per step), with Duration/Tokens summed over every
// step, and each step demoted to KindActivity.
func TestBuildOpencodeEventsMultiStepTurn(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "u1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: msgJSON("user", 1000, 0, "")},
		{ID: "step1", SessionID: "ses1", TimeCreated: 1010, TimeUpdated: 1050, Data: msgJSON("assistant", 1010, 1050, "tool-calls")},
		{ID: "step2", SessionID: "ses1", TimeCreated: 1055, TimeUpdated: 1090, Data: msgJSON("assistant", 1055, 1090, "tool-calls")},
		{ID: "final", SessionID: "ses1", TimeCreated: 1095, TimeUpdated: 1120, Data: msgJSON("assistant", 1095, 1120, "stop")},
	}
	partRows := []opencodePartRow{
		{ID: "p_user", MessageID: "u1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("text", "", "do the thing", "", "")},
		{ID: "p_tool1", MessageID: "step1", SessionID: "ses1", TimeCreated: 1020, TimeUpdated: 1020, Data: partJSON("tool", "bash", "", "completed", "")},
		{ID: "p_tool2", MessageID: "step2", SessionID: "ses1", TimeCreated: 1060, TimeUpdated: 1060, Data: partJSON("tool", "read", "", "completed", "")},
		{ID: "p_final", MessageID: "final", SessionID: "ses1", TimeCreated: 1110, TimeUpdated: 1110, Data: partJSON("text", "", "all done", "", "")},
	}
	// As queryTurns would compute: one turn spanning both steps and the
	// terminal message, tokens/cost summed across all three, duration from
	// the preceding user message's created time to the terminal completion.
	turnRows := []opencodeTurnRow{
		{
			SessionID: "ses1", TerminalID: "final", TerminalTimeUpdated: 1120,
			Completed: 1120, StartTime: 1000,
			SumIn: 170, SumOut: 23,
		},
	}

	events, newOffset := buildOpencodeEvents(inst, msgRows, partRows, turnRows)
	if newOffset != 1120 {
		t.Fatalf("newOffset = %d, want 1120", newOffset)
	}

	var turnEnds, assistantMsgs, userMsgs, stepActivities, toolActivities int
	for _, ev := range events {
		switch {
		case ev.Kind == KindTurnEnd:
			turnEnds++
			if ev.Duration != 120*time.Millisecond {
				t.Errorf("turn Duration = %v, want 120ms (terminal completed 1120 - user created 1000)", ev.Duration)
			}
			if ev.Tokens.InputTokens != 170 || ev.Tokens.OutputTokens != 23 {
				t.Errorf("turn Tokens = %+v, want summed across all steps", ev.Tokens)
			}
		case ev.Kind == KindAssistantMsg:
			assistantMsgs++
			if ev.Excerpt != "all done" {
				t.Errorf("assistant excerpt = %q", ev.Excerpt)
			}
		case ev.Kind == KindUserMessage:
			userMsgs++
		case ev.Kind == KindActivity && ev.Tool != "":
			toolActivities++
		case ev.Kind == KindActivity:
			stepActivities++
		}
	}
	if turnEnds != 1 {
		t.Fatalf("turnEnds = %d, want exactly 1 (not one per step)", turnEnds)
	}
	if assistantMsgs != 1 || userMsgs != 1 {
		t.Fatalf("assistantMsgs=%d userMsgs=%d, want 1 each", assistantMsgs, userMsgs)
	}
	if stepActivities != 2 {
		t.Fatalf("stepActivities = %d, want 2 (one KindActivity per tool-calls step)", stepActivities)
	}
	if toolActivities != 2 {
		t.Fatalf("toolActivities = %d, want 2 (the completed tool parts under each step)", toolActivities)
	}
}

func TestBuildOpencodeEventsStepMessageAloneBecomesActivity(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "step1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1050, Data: msgJSON("assistant", 1000, 1050, "tool-calls")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, nil, nil)
	if len(events) != 1 || events[0].Kind != KindActivity {
		t.Fatalf("events = %+v, want a single activity event for the step", events)
	}
}

func TestBuildOpencodeEventsAPIError(t *testing.T) {
	inst := Instance{Name: "probe"}
	turnRows := []opencodeTurnRow{
		{
			SessionID: "ses1", TerminalID: "msg1", TerminalTimeUpdated: 1200,
			Completed: 1200, StartTime: 1000,
			ErrorName: "APIError", ErrorMessage: "Bad Gateway",
		},
	}
	events, _ := buildOpencodeEvents(inst, nil, nil, turnRows)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if events[0].Kind != KindAPIError {
		t.Fatalf("Kind = %q, want api_error", events[0].Kind)
	}
	if events[0].Excerpt != "Bad Gateway" {
		t.Fatalf("Excerpt = %q", events[0].Excerpt)
	}
	if events[0].Duration != 200*time.Millisecond {
		t.Fatalf("Duration = %v, want 200ms", events[0].Duration)
	}
}

func TestBuildOpencodeEventsAuthError(t *testing.T) {
	inst := Instance{Name: "probe"}
	turnRows := []opencodeTurnRow{
		{
			SessionID: "ses1", TerminalID: "msg1", TerminalTimeUpdated: 1200,
			Completed: 1200, StartTime: 1000,
			ErrorName: "APIError", ErrorMessage: "Invalid token (request id: abc)",
		},
	}
	events, _ := buildOpencodeEvents(inst, nil, nil, turnRows)
	if len(events) != 1 || events[0].Kind != KindAuthError {
		t.Fatalf("events = %+v, want single auth_error", events)
	}
}

func TestBuildOpencodeEventsUserMessage(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: msgJSON("user", 1000, 0, "")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("text", "", "please do X", "", "")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, partRows, nil)
	if len(events) != 1 || events[0].Kind != KindUserMessage || events[0].Excerpt != "please do X" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildOpencodeEventsToolError(t *testing.T) {
	inst := Instance{Name: "probe"}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("tool", "bash", "", "error", "exit status 1")},
	}
	events, _ := buildOpencodeEvents(inst, nil, partRows, nil)
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
	events, _ := buildOpencodeEvents(inst, nil, partRows, nil)
	if len(events) != 1 || events[0].Kind != KindActivity || events[0].Tool != "read" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildOpencodeEventsCompaction(t *testing.T) {
	inst := Instance{Name: "probe"}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: `{"type":"compaction"}`},
	}
	events, _ := buildOpencodeEvents(inst, nil, partRows, nil)
	if len(events) != 1 || events[0].Kind != KindCompaction {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildOpencodeEventsInProgressAssistantIsActivity(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1100, Data: msgJSON("assistant", 1000, 0, "")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1100, TimeUpdated: 1100, Data: partJSON("text", "", "partial...", "", "")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, partRows, nil)
	if len(events) != 1 || events[0].Kind != KindActivity || events[0].Excerpt != "partial..." {
		t.Fatalf("events = %+v, want a single activity event for interim text", events)
	}
}

func TestBuildOpencodeEventsExcerptIsRedacted(t *testing.T) {
	inst := Instance{Name: "probe"}
	msgRows := []opencodeMessageRow{
		{ID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: msgJSON("user", 1000, 0, "")},
	}
	partRows := []opencodePartRow{
		{ID: "p1", MessageID: "msg1", SessionID: "ses1", TimeCreated: 1000, TimeUpdated: 1000, Data: partJSON("text", "", "here is sk-ant-abcdef123456789012 ok", "", "")},
	}
	events, _ := buildOpencodeEvents(inst, msgRows, partRows, nil)
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

	// New activity after the first poll: a full user -> assistant turn
	// (single-step: the assistant message has no "finish" field at all,
	// same as real rows where finish is simply absent, which still ends
	// the turn since it isn't "tool-calls").
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
			// Duration is completion (400) minus the *preceding user
			// message's* created time (300), per the SQL turn query, not
			// the assistant message's own created time (310).
			if ev.Duration != 100*time.Millisecond {
				t.Errorf("Duration = %v, want 100ms (400 - preceding user's 300)", ev.Duration)
			}
			if ev.Tokens.InputTokens != 5 || ev.Tokens.OutputTokens != 7 {
				t.Errorf("Tokens = %+v", ev.Tokens)
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

// TestOpencodeCollectorMultiStepTurnEndToEnd exercises the real SQL turn
// grouping/aggregation (queryTurns) against the actual sqlite3 CLI: a user
// message followed by two "tool-calls" steps and a "stop" message that ends
// the turn must produce exactly one turn_end (not three), with duration and
// tokens summed across all three assistant rows.
func TestOpencodeCollectorMultiStepTurnEndToEnd(t *testing.T) {
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
  ('m_user', 'ses1', 1000, 1000, '{"role":"user","time":{"created":1000}}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p_user', 'm_user', 'ses1', 1000, 1000, '{"type":"text","text":"do the multi-step thing"}');
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_step1', 'ses1', 1010, 1050, '{"role":"assistant","time":{"created":1010,"completed":1050},"finish":"tool-calls","tokens":{"input":100,"output":10,"cache":{"write":0,"read":0}},"cost":0.0005}');
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_step2', 'ses1', 1055, 1090, '{"role":"assistant","time":{"created":1055,"completed":1090},"finish":"tool-calls","tokens":{"input":50,"output":5,"cache":{"write":0,"read":0}},"cost":0.0003}');
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('m_final', 'ses1', 1095, 1120, '{"role":"assistant","time":{"created":1095,"completed":1120},"finish":"stop","tokens":{"input":20,"output":8,"cache":{"write":0,"read":0}},"cost":0.0002}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p_final', 'm_final', 'ses1', 1110, 1110, '{"type":"text","text":"multi-step done"}');
`)

	// Backfill: true so this single poll sees the whole fixture (the
	// offset/floor behavior itself is covered separately by
	// TestOpencodeCollectorEndToEnd and TestOpencodeCollectorBackfill; this
	// test is about the turn grouping/aggregation SQL).
	c := &OpencodeCollector{Backfill: true}
	inst := Instance{Name: "probe", Home: home, Workdir: "/work"}
	events, err := c.Poll(context.Background(), inst, memOffsets{})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	var turnEnds, assistantMsgs, stepActivities int
	var turnEndTokensIn, turnEndTokensOut int64
	var turnEndDuration time.Duration
	for _, ev := range events {
		switch ev.Kind {
		case KindTurnEnd:
			turnEnds++
			turnEndTokensIn = ev.Tokens.InputTokens
			turnEndTokensOut = ev.Tokens.OutputTokens
			turnEndDuration = ev.Duration
		case KindAssistantMsg:
			assistantMsgs++
			if ev.Excerpt != "multi-step done" {
				t.Errorf("assistant excerpt = %q", ev.Excerpt)
			}
		case KindActivity:
			if ev.Tool == "" {
				stepActivities++
			}
		}
	}
	if turnEnds != 1 {
		t.Fatalf("turnEnds = %d, want exactly 1 across 3 assistant messages (2 steps + terminal)", turnEnds)
	}
	if assistantMsgs != 1 {
		t.Fatalf("assistantMsgs = %d, want 1", assistantMsgs)
	}
	if stepActivities != 2 {
		t.Fatalf("stepActivities = %d, want 2 (one per tool-calls step)", stepActivities)
	}
	if turnEndTokensIn != 170 || turnEndTokensOut != 23 {
		t.Fatalf("turn tokens = in:%d out:%d, want in:170 out:23 (summed across all 3 steps)", turnEndTokensIn, turnEndTokensOut)
	}
	if turnEndDuration != 120*time.Millisecond {
		t.Fatalf("turn Duration = %v, want 120ms (1120 - user's 1000)", turnEndDuration)
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
