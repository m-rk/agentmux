package threadwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// fakePaneViewer is a fixed PaneViewer for tests, recording every instance
// it was asked to view.
type fakePaneViewer struct {
	content string
	err     error
	calls   []string
}

func (f *fakePaneViewer) ViewPane(ctx context.Context, instance string) (string, error) {
	f.calls = append(f.calls, instance)
	if f.err != nil {
		return "", f.err
	}
	return f.content, nil
}

// --- appendPaneEvidence ---

func TestAppendPaneEvidence_AppendsTailForAwaitingUserAndStalledTurn(t *testing.T) {
	pane := &fakePaneViewer{content: "some old line\n\nShould I continue with the deploy?\n"}
	r := &Runner{PaneViewer: pane}

	for _, code := range []string{CodeAwaitingUser, CodeStalledTurn} {
		sig := Signal{Instance: "inst1", Code: code, Evidence: "turn ended"}
		r.appendPaneEvidence(context.Background(), &sig)
		if !strings.Contains(sig.Evidence, "--- pane ---") {
			t.Errorf("%s: expected a pane-tail marker in evidence, got %q", code, sig.Evidence)
		}
		if !strings.Contains(sig.Evidence, "Should I continue with the deploy?") {
			t.Errorf("%s: expected pane content in evidence, got %q", code, sig.Evidence)
		}
		if !strings.Contains(sig.Evidence, "turn ended") {
			t.Errorf("%s: expected the original evidence to be preserved, got %q", code, sig.Evidence)
		}
	}
}

func TestAppendPaneEvidence_SkipsOtherCodesAndResolved(t *testing.T) {
	pane := &fakePaneViewer{content: "Should I continue?"}
	r := &Runner{PaneViewer: pane}

	sig := Signal{Instance: "inst1", Code: CodeErrorLoop, Evidence: "boom"}
	r.appendPaneEvidence(context.Background(), &sig)
	if sig.Evidence != "boom" {
		t.Errorf("expected a code other than awaiting_user/stalled_turn to be left alone, got %q", sig.Evidence)
	}

	resolved := Signal{Instance: "inst1", Code: CodeAwaitingUser, Resolved: true, Evidence: "boom"}
	r.appendPaneEvidence(context.Background(), &resolved)
	if resolved.Evidence != "boom" {
		t.Errorf("expected a resolved signal to be left alone, got %q", resolved.Evidence)
	}

	if len(pane.calls) != 0 {
		t.Errorf("expected ViewPane never called for skipped signals, got %d calls", len(pane.calls))
	}
}

func TestAppendPaneEvidence_NoPaneViewer_LeavesEvidenceUnchanged(t *testing.T) {
	r := &Runner{}
	sig := Signal{Instance: "inst1", Code: CodeAwaitingUser, Evidence: "boom"}
	r.appendPaneEvidence(context.Background(), &sig)
	if sig.Evidence != "boom" {
		t.Errorf("expected evidence unchanged with no PaneViewer, got %q", sig.Evidence)
	}
}

func TestAppendPaneEvidence_ViewPaneError_LeavesEvidenceUnchanged(t *testing.T) {
	pane := &fakePaneViewer{err: errors.New("boom")}
	r := &Runner{PaneViewer: pane}
	sig := Signal{Instance: "inst1", Code: CodeAwaitingUser, Evidence: "boom"}
	r.appendPaneEvidence(context.Background(), &sig)
	if sig.Evidence != "boom" {
		t.Errorf("expected evidence unchanged on a ViewPane error, got %q", sig.Evidence)
	}
}

func TestAppendPaneEvidence_OnlyLastNNonEmptyLines(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	pane := &fakePaneViewer{content: strings.Join(lines, "\n")}
	r := &Runner{PaneViewer: pane}

	sig := Signal{Instance: "inst1", Code: CodeAwaitingUser}
	r.appendPaneEvidence(context.Background(), &sig)

	if strings.Contains(sig.Evidence, "line 0\n") || strings.Contains(sig.Evidence, "line 9\n") {
		t.Errorf("expected only the last 20 non-empty lines to be kept, got %q", sig.Evidence)
	}
	if !strings.Contains(sig.Evidence, "line 29") {
		t.Errorf("expected the last line to be present, got %q", sig.Evidence)
	}
	if !strings.Contains(sig.Evidence, "line 10") {
		t.Errorf("expected the 20th-from-last line to be present, got %q", sig.Evidence)
	}
}

// --- pane tail through a full Cycle: reaches Judge, and rule-suppression
// re-tiers to insight ---

func TestRunnerCycle_PaneTailAppendedBeforeJudge_RuleRetiersToInsight(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := NewStore(StateDir(home))
	if err != nil {
		t.Fatal(err)
	}
	offsets, err := LoadOffsets(StateDir(home))
	if err != nil {
		t.Fatal(err)
	}

	// Agent "generic" has no structured collector (newCollectorForAgent),
	// so Cycle falls back to the pane-hash heartbeat, giving the Detector a
	// KindActivity event (and so an open turn) straight from the fake pane.
	pbi := &pb.Instance{Name: "inst1", Agent: "generic", Workdir: "/repo", Status: pb.Status_STATUS_RUNNING}
	lister := &fakeLister{instances: []*pb.Instance{pbi}}
	pane := &fakePaneViewer{content: "Finished the task. All done."}
	judge := &fakeJudge{judgment: Judgment{}} // no Err, but NeedsHumanNow 0 fails the awaiting_user gate
	sender := &fakeSender{}
	cfg := DefaultConfig()

	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	runner := &Runner{
		Config:     cfg,
		Store:      store,
		Offsets:    offsets,
		Lister:     lister,
		Alerter:    NewAlerter(cfg, "test-host", sender.send, nil),
		Judge:      judge,
		PaneViewer: pane,
		Clock:      func() time.Time { return now },
	}

	ctx := context.Background()
	if err := runner.Cycle(ctx); err != nil {
		t.Fatalf("first Cycle: %v", err)
	}

	// Flip the instance to idle. This produces a KindStatus event, which
	// (like the pane-fallback's KindActivity events) carries no Thread, so
	// it touches the same thread state and — like any event — resets its
	// silence clock. Do this a little after the first cycle, then advance
	// well past AwaitingUserAfter with no further status change or pane
	// change before ticking again, so the open-turn idle-prompt path
	// (detect.go's Tick) has a clean silence window to fire on.
	now = now.Add(10 * time.Second)
	pbi.Status = pb.Status_STATUS_IDLE
	if err := runner.Cycle(ctx); err != nil {
		t.Fatalf("second Cycle: %v", err)
	}

	now = now.Add(11 * time.Minute)
	if err := runner.Cycle(ctx); err != nil {
		t.Fatalf("third Cycle: %v", err)
	}

	if len(judge.calls) != 1 {
		t.Fatalf("expected exactly one Judge call, got %d: %+v", len(judge.calls), judge.calls)
	}
	judged := judge.calls[0]
	if judged.Code != CodeAwaitingUser {
		t.Fatalf("expected the judged signal to be awaiting_user, got %q", judged.Code)
	}
	if !strings.Contains(judged.Evidence, "--- pane ---") || !strings.Contains(judged.Evidence, "Finished the task") {
		t.Errorf("expected the judge to see the pane tail in Evidence (appended before judging), got %q", judged.Evidence)
	}

	signals, err := runner.Store.ReadSignals(testReadSince, testReadUntil)
	if err != nil {
		t.Fatal(err)
	}
	var awaiting *Signal
	for i := range signals {
		if signals[i].Code == CodeAwaitingUser && !signals[i].Resolved {
			awaiting = &signals[i]
		}
	}
	if awaiting == nil {
		t.Fatalf("want an awaiting_user signal, got %+v", signals)
	}
	if !strings.Contains(awaiting.Evidence, "--- pane ---") {
		t.Errorf("expected the stored signal's Evidence to include the pane tail, got %q", awaiting.Evidence)
	}
	if awaiting.Tier != TierInsight {
		t.Errorf("expected the rule-suppressed signal to be re-tiered to insight, got tier %q", awaiting.Tier)
	}
	if sender.count() != 0 {
		t.Errorf("expected no page sent (the rule found no question in the evidence), got %d: %v", sender.count(), sender.messages)
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
