package threadwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

// testReadSince/testReadUntil bound Store reads in this file's tests. They
// must cover both real wall-clock time (KindStatus/session_exit events,
// whose Time comes from Runner's now()) and the fixed 2030 timestamps the
// synthetic claude-code transcript lines use.
var (
	testReadSince = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	testReadUntil = time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
)

// fakeLister is a fixed InstanceLister for tests.
type fakeLister struct {
	instances []*pb.Instance
}

func (f *fakeLister) ListInstances(ctx context.Context) ([]*pb.Instance, error) {
	return f.instances, nil
}

// fakeJudge is a Judge (and InsightJudge) that returns a fixed Judgment,
// recording every call it was asked to make.
type fakeJudge struct {
	judgment     Judgment
	calls        []Signal
	insightCalls []Signal
}

func (j *fakeJudge) Judge(ctx context.Context, sig Signal, recent []Event) Judgment {
	j.calls = append(j.calls, sig)
	return j.judgment
}

func (j *fakeJudge) JudgeInsight(ctx context.Context, sig Signal, recent []Event) Judgment {
	j.insightCalls = append(j.insightCalls, sig)
	return j.judgment
}

// writeClaudeLines appends lines (already-formatted JSONL, one per line) to
// path, creating the file if needed.
func writeClaudeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// apiErrorLine is one Claude Code transcript record that ClaudeCollector
// maps to a KindAPIError event.
func apiErrorLine(ts, text string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","sessionId":"sess-1","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
}

// newTestRunner builds a Runner wired to a temp HOME containing a claude-code
// project directory for workdir, a fresh Store/FileOffsets rooted under that
// HOME's state dir, a fakeLister reporting one running claude-code instance,
// and a fakeSender (from alert_test.go). It returns the Runner, HOME, the
// project directory the transcript lives in, and the sender.
func newTestRunner(t *testing.T, workdir string, judge Judge) (runner *Runner, home, projDir string, sender *fakeSender) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)

	projDir = filepath.Join(home, ".claude", "projects", claudeProjectSlug(workdir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(StateDir(home))
	if err != nil {
		t.Fatal(err)
	}
	offsets, err := LoadOffsets(StateDir(home))
	if err != nil {
		t.Fatal(err)
	}

	lister := &fakeLister{instances: []*pb.Instance{
		{Name: "inst1", Agent: "claude-code", Workdir: workdir, Status: pb.Status_STATUS_RUNNING},
	}}
	sender = &fakeSender{}
	cfg := DefaultConfig()

	runner = &Runner{
		Config:  cfg,
		Store:   store,
		Offsets: offsets,
		Lister:  lister,
		Alerter: NewAlerter(cfg, "test-host", sender.send, nil),
		Judge:   judge,
	}
	return runner, home, projDir, sender
}

func TestRunnerCycleFullFlow(t *testing.T) {
	const workdir = "/repo/app"
	judge := &fakeJudge{judgment: Judgment{Urgency: 5, UrgencyConf: 0.9}}
	runner, home, projDir, sender := newTestRunner(t, workdir, judge)
	transcript := filepath.Join(projDir, "session.jsonl")

	var decisions []Decision
	runner.OnDecision = func(sig Signal, d Decision) {
		decisions = append(decisions, d)
	}

	ctx := context.Background()

	// The transcript file exists but is empty before the first cycle, so
	// that cycle establishes an offset of 0 for it (a collector always
	// starts an unseen file at EOF, per Backfill=false; an empty file's EOF
	// is byte 0). That models a session that's just started, and lets the
	// second cycle's appended lines actually be read as new content instead
	// of being treated as "already there" and skipped.
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runner.Cycle(ctx); err != nil {
		t.Fatalf("first Cycle: %v", err)
	}

	events, err := runner.Store.ReadEvents(testReadSince, testReadUntil)
	if err != nil {
		t.Fatal(err)
	}
	foundStatus := false
	for _, e := range events {
		if e.Kind == KindStatus && e.Instance == "inst1" && e.Status == "running" {
			foundStatus = true
		}
	}
	if !foundStatus {
		t.Errorf("first cycle: want a KindStatus event for inst1, got %+v", events)
	}

	// Now three consecutive API errors land in the transcript, which
	// error_loop should catch on the third (APIErrorLoop defaults to 3, an
	// Observe-time trigger with no time-based threshold to wait out).
	writeClaudeLines(t, transcript,
		apiErrorLine("2030-01-01T00:00:00Z", "boom 1"),
		apiErrorLine("2030-01-01T00:00:01Z", "boom 2"),
		apiErrorLine("2030-01-01T00:00:02Z", "boom 3"),
	)

	if err := runner.Cycle(ctx); err != nil {
		t.Fatalf("second Cycle: %v", err)
	}

	signals, err := runner.Store.ReadSignals(testReadSince, testReadUntil)
	if err != nil {
		t.Fatal(err)
	}
	var errorLoop *Signal
	for i := range signals {
		if signals[i].Code == CodeErrorLoop {
			errorLoop = &signals[i]
		}
	}
	if errorLoop == nil {
		t.Fatalf("want an error_loop signal, got %+v", signals)
	}
	if errorLoop.Tier != TierIntervene {
		t.Errorf("error_loop tier = %q, want %q", errorLoop.Tier, TierIntervene)
	}
	if errorLoop.Judgment == nil {
		t.Errorf("error_loop signal has no Judgment; want the fake judge's verdict recorded")
	} else if errorLoop.Judgment.Urgency != 5 {
		t.Errorf("error_loop Judgment.Urgency = %v, want 5", errorLoop.Judgment.Urgency)
	}
	if len(judge.calls) == 0 {
		t.Errorf("want the fake judge to have been called for the intervene signal")
	}

	if sender.count() != 1 {
		t.Fatalf("want exactly one Discord message sent, got %d: %v", sender.count(), sender.messages)
	}

	foundPaged := false
	for _, d := range decisions {
		if d.Page {
			foundPaged = true
		}
	}
	if !foundPaged {
		t.Errorf("want OnDecision to see a paged decision, got %+v", decisions)
	}

	// Offsets advanced past the three lines just written and were saved to
	// disk (Offsets.Save is called at the end of every Cycle).
	key := "claude:" + transcript
	off, ok := runner.Offsets.Get(key)
	if !ok || off == 0 {
		t.Errorf("want a non-zero offset recorded for %s, got %d (known=%v)", key, off, ok)
	}
	if _, err := os.Stat(filepath.Join(StateDir(home), "offsets.json")); err != nil {
		t.Errorf("want offsets.json persisted: %v", err)
	}
}

func TestRunnerRestartDoesNotReplayEvents(t *testing.T) {
	const workdir = "/repo/app"
	runner, home, projDir, sender := newTestRunner(t, workdir, nil)
	transcript := filepath.Join(projDir, "session.jsonl")

	// Write a full transcript before thread watch ever runs (as if the
	// instance had been active for a while already) and confirm the first
	// cycle does not replay it as a flood of events (ClaudeCollector's
	// Backfill defaults to false: unseen files start at EOF).
	writeClaudeLines(t, transcript,
		apiErrorLine("2030-01-01T00:00:00Z", "boom 1"),
		apiErrorLine("2030-01-01T00:00:01Z", "boom 2"),
		apiErrorLine("2030-01-01T00:00:02Z", "boom 3"),
	)

	ctx := context.Background()
	if err := runner.Cycle(ctx); err != nil {
		t.Fatalf("first Cycle: %v", err)
	}
	if sender.count() != 0 {
		t.Fatalf("pre-existing transcript content replayed as alerts: %v", sender.messages)
	}

	// Simulate a daemon restart: a brand new Runner (fresh in-memory
	// collector state) but the same persisted offsets. It must not re-read
	// what the first Runner already consumed.
	offsets2, err := LoadOffsets(StateDir(home))
	if err != nil {
		t.Fatal(err)
	}
	store2, err := NewStore(StateDir(home))
	if err != nil {
		t.Fatal(err)
	}
	lister := &fakeLister{instances: []*pb.Instance{
		{Name: "inst1", Agent: "claude-code", Workdir: workdir, Status: pb.Status_STATUS_RUNNING},
	}}
	sender2 := &fakeSender{}
	cfg := DefaultConfig()
	runner2 := &Runner{
		Config:  cfg,
		Store:   store2,
		Offsets: offsets2,
		Lister:  lister,
		Alerter: NewAlerter(cfg, "test-host", sender2.send, nil),
	}
	if err := runner2.Cycle(ctx); err != nil {
		t.Fatalf("restarted Cycle: %v", err)
	}
	if sender2.count() != 0 {
		t.Errorf("restart replayed already-consumed transcript content: %v", sender2.messages)
	}
}
