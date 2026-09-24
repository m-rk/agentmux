package threadwatch

import (
	"fmt"
	"testing"
	"time"
)

// wantSig is what a test expects one emitted Signal to look like. Tier is
// derived from Code (see tierOf) so tests only spell out what varies.
type wantSig struct {
	Code     string
	Resolved bool
}

func tierOf(code string) string {
	switch code {
	case CodeAwaitingUser, CodeErrorLoop, CodeAuthFailed, CodeStalledTurn, CodeDiedMidTurn:
		return TierIntervene
	default:
		return TierInsight
	}
}

func assertSignals(t *testing.T, step string, got []Signal, want []wantSig) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d signals %+v, want %d %+v", step, len(got), got, len(want), want)
	}
	for i, w := range want {
		g := got[i]
		if g.Code != w.Code || g.Resolved != w.Resolved {
			t.Errorf("%s: signal %d = {%s resolved=%v}, want {%s resolved=%v}", step, i, g.Code, g.Resolved, w.Code, w.Resolved)
		}
		if g.Tier != tierOf(w.Code) {
			t.Errorf("%s: signal %d tier = %q, want %q", step, i, g.Tier, tierOf(w.Code))
		}
		if g.Reason == "" {
			t.Errorf("%s: signal %d has empty Reason", step, i)
		}
		if g.Instance == "" {
			t.Errorf("%s: signal %d has empty Instance", step, i)
		}
	}
}

func excerptFor(i int) string {
	return fmt.Sprintf("exit 1 (attempt %d)", i)
}

func day(hourOffset int) time.Time {
	return time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(hourOffset) * time.Hour)
}

// --- awaiting_user ---

func TestAwaitingUser(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	instances := []Instance{{Name: "i1", Status: "idle"}}

	got := d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindTurnEnd, Excerpt: "done, want me to also update the docs?"})
	assertSignals(t, "turn_end anchors the wait", got, nil)

	got = d.Tick(t0.Add(9*time.Minute), instances)
	assertSignals(t, "under threshold", got, nil)

	got = d.Tick(t0.Add(11*time.Minute), instances)
	assertSignals(t, "crosses threshold, fires once", got, []wantSig{{Code: CodeAwaitingUser}})
	if got[0].Evidence == "" {
		t.Errorf("expected non-empty evidence (last assistant excerpt)")
	}

	got = d.Tick(t0.Add(12*time.Minute), instances)
	assertSignals(t, "no duplicate on next tick", got, nil)

	got = d.Observe(Event{Time: t0.Add(15 * time.Minute), Instance: "i1", Thread: "t1", Kind: KindUserMessage})
	assertSignals(t, "resolves on user reply", got, []wantSig{{Code: CodeAwaitingUser, Resolved: true}})
}

func TestAwaitingUserRequiresIdleStatus(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindTurnEnd})

	got := d.Tick(t0.Add(20*time.Minute), []Instance{{Name: "i1", Status: "running"}})
	assertSignals(t, "running instance never fires awaiting_user", got, nil)
}

func TestLongWait(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindTurnEnd, Excerpt: "ok?"})

	got := d.Tick(t0.Add(11*time.Minute), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "awaiting_user fires", got, []wantSig{{Code: CodeAwaitingUser}})

	reply := t0.Add(11 * time.Minute).Add(3 * time.Hour) // > LongWaitAfter (2h) past the alert
	got = d.Observe(Event{Time: reply, Instance: "i1", Thread: "t1", Kind: KindUserMessage})
	assertSignals(t, "resolves and reports long_wait", got, []wantSig{
		{Code: CodeAwaitingUser, Resolved: true},
		{Code: CodeLongWait},
	})
}

func TestAwaitingUserFromAssistantMsgAnchor(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)

	// No turn_end yet, but a final assistant message also anchors the wait.
	got := d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindAssistantMsg, Excerpt: "should I proceed with the migration?"})
	assertSignals(t, "assistant_msg anchors the wait", got, nil)

	got = d.Tick(t0.Add(11*time.Minute), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "fires from the assistant_msg anchor", got, []wantSig{{Code: CodeAwaitingUser}})
	if got[0].Evidence == "" {
		t.Errorf("expected evidence from the assistant_msg excerpt")
	}
}

func TestActivityClearsAwaitingWithoutResolving(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindTurnEnd})

	got := d.Observe(Event{Time: t0.Add(time.Minute), Instance: "i1", Thread: "t1", Kind: KindActivity})
	assertSignals(t, "activity before threshold: nothing to resolve", got, nil)

	got = d.Tick(t0.Add(20*time.Minute), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "activity cleared the awaiting window, never fires", got, nil)
}

// --- error_loop ---

func TestErrorLoopAPIConsecutive(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	for i := 0; i < 2; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(i) * time.Second), Instance: inst, Thread: thr, Kind: KindAPIError, Excerpt: "rate limited"})
		assertSignals(t, "sub-threshold api error", got, nil)
	}
	got := d.Observe(Event{Time: t0.Add(3 * time.Second), Instance: inst, Thread: thr, Kind: KindAPIError, Excerpt: "rate limited"})
	assertSignals(t, "3rd consecutive api error triggers", got, []wantSig{{Code: CodeErrorLoop}})

	got = d.Observe(Event{Time: t0.Add(4 * time.Second), Instance: inst, Thread: thr, Kind: KindAPIError})
	assertSignals(t, "4th: no duplicate", got, nil)

	got = d.Observe(Event{Time: t0.Add(5 * time.Second), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "resolves and reports recovery on turn_end", got, []wantSig{
		{Code: CodeErrorLoop, Resolved: true},
		{Code: CodeRecoveredErrors},
	})
}

func TestErrorLoopAPIResetByActivity(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindAPIError})
	d.Observe(Event{Time: t0.Add(time.Second), Instance: inst, Thread: thr, Kind: KindAPIError})

	got := d.Observe(Event{Time: t0.Add(2 * time.Second), Instance: inst, Thread: thr, Kind: KindActivity})
	assertSignals(t, "activity breaks the consecutive streak", got, nil)

	got = d.Observe(Event{Time: t0.Add(3 * time.Second), Instance: inst, Thread: thr, Kind: KindAPIError})
	assertSignals(t, "1st after reset", got, nil)
	got = d.Observe(Event{Time: t0.Add(4 * time.Second), Instance: inst, Thread: thr, Kind: KindAPIError})
	assertSignals(t, "2nd after reset", got, nil)
	got = d.Observe(Event{Time: t0.Add(5 * time.Second), Instance: inst, Thread: thr, Kind: KindAPIError})
	assertSignals(t, "3rd after reset triggers", got, []wantSig{{Code: CodeErrorLoop}})
}

func TestErrorLoopTool(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	// Distinct excerpts per attempt so this exercises error_loop without
	// also crossing the (Tool, Excerpt) retry_thrash threshold.
	for i := 0; i < 3; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(i) * time.Second), Instance: inst, Thread: thr, Kind: KindToolError, Tool: "bash", Excerpt: excerptFor(i)})
		assertSignals(t, "sub-threshold tool error", got, nil)
	}
	// Tool-error counting is scoped to "one open turn", not consecutiveness,
	// so intervening activity should not reset it.
	got := d.Observe(Event{Time: t0.Add(4 * time.Second), Instance: inst, Thread: thr, Kind: KindActivity})
	assertSignals(t, "activity does not reset tool error count", got, nil)

	got = d.Observe(Event{Time: t0.Add(5 * time.Second), Instance: inst, Thread: thr, Kind: KindToolError, Tool: "bash", Excerpt: excerptFor(3)})
	assertSignals(t, "4th tool error triggers", got, []wantSig{{Code: CodeErrorLoop}})

	got = d.Observe(Event{Time: t0.Add(6 * time.Second), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "resolves and reports recovery on turn_end", got, []wantSig{
		{Code: CodeErrorLoop, Resolved: true},
		{Code: CodeRecoveredErrors},
	})

	// A new turn starts the per-tool count fresh.
	for i := 0; i < 3; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(7+i) * time.Second), Instance: inst, Thread: thr, Kind: KindToolError, Tool: "bash", Excerpt: excerptFor(10 + i)})
		assertSignals(t, "sub-threshold in new turn", got, nil)
	}
}

// --- auth_failed ---

func TestAuthFailed(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"

	got := d.Observe(Event{Time: t0, Instance: inst, Thread: "t1", Kind: KindAuthError, Excerpt: "401 unauthorized"})
	assertSignals(t, "first auth error fires", got, []wantSig{{Code: CodeAuthFailed}})

	got = d.Observe(Event{Time: t0.Add(10 * time.Minute), Instance: inst, Thread: "t2", Kind: KindAuthError})
	assertSignals(t, "within cooldown, no duplicate even from another thread", got, nil)

	got = d.Observe(Event{Time: t0.Add(20 * time.Minute), Instance: inst, Thread: "t2", Kind: KindTurnEnd})
	assertSignals(t, "resolves on any thread's successful turn_end", got, []wantSig{{Code: CodeAuthFailed, Resolved: true}})

	got = d.Observe(Event{Time: t0.Add(2 * time.Hour), Instance: inst, Thread: "t1", Kind: KindAuthError})
	assertSignals(t, "fires again after the cooldown window", got, []wantSig{{Code: CodeAuthFailed}})
}

// --- stalled_turn ---

func TestStalledTurn(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"
	instances := []Instance{{Name: inst, Status: "running"}}

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUserMessage})

	got := d.Tick(t0.Add(10*time.Minute), instances)
	assertSignals(t, "under threshold", got, nil)

	got = d.Tick(t0.Add(16*time.Minute), instances)
	assertSignals(t, "stalled fires once", got, []wantSig{{Code: CodeStalledTurn}})

	got = d.Tick(t0.Add(17*time.Minute), instances)
	assertSignals(t, "no duplicate", got, nil)

	got = d.Observe(Event{Time: t0.Add(18 * time.Minute), Instance: inst, Thread: thr, Kind: KindActivity})
	assertSignals(t, "resolves when events resume", got, []wantSig{{Code: CodeStalledTurn, Resolved: true}})
}

func TestStalledTurnRequiresRunningStatus(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindUserMessage})

	got := d.Tick(t0.Add(20*time.Minute), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "idle status never triggers stalled_turn", got, nil)
}

// --- died_mid_turn ---

func TestDiedMidTurnSessionExit(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"
	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUserMessage})

	got := d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Kind: KindSessionExit})
	assertSignals(t, "died mid turn", got, []wantSig{{Code: CodeDiedMidTurn}})

	got = d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Kind: KindSessionExit})
	assertSignals(t, "turn already closed, no duplicate", got, nil)
}

func TestDiedMidTurnStatusStopped(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"
	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUserMessage})

	got := d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Kind: KindStatus, Status: "stopped"})
	assertSignals(t, "died via status stopped", got, []wantSig{{Code: CodeDiedMidTurn}})
}

func TestDiedMidTurnOnlyWhileOpen(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"
	// No open turn (thread never had a user_message/activity).
	got := d.Observe(Event{Time: t0, Instance: inst, Kind: KindSessionExit})
	assertSignals(t, "no open turn, nothing to die", got, nil)
}

// --- slow_turn ---

func TestSlowTurnFallbackBeforeBaseline(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"

	got := d.Observe(Event{Time: t0, Instance: inst, Thread: "t1", Kind: KindTurnEnd, Duration: 6 * time.Minute})
	assertSignals(t, "over SlowTurnMin but under 2x fallback floor", got, nil)

	got = d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: "t2", Kind: KindTurnEnd, Duration: 11 * time.Minute})
	assertSignals(t, "over the 2x fallback floor", got, []wantSig{{Code: CodeSlowTurn}})
}

func TestSlowTurnUsesRollingP90OnceEnoughSamples(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"

	for i := 0; i < 20; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(i) * time.Minute), Instance: inst, Thread: "base", Kind: KindTurnEnd, Duration: time.Minute})
		assertSignals(t, "baseline sample", got, nil)
	}

	got := d.Observe(Event{Time: t0.Add(21 * time.Minute), Instance: inst, Thread: "t1", Kind: KindTurnEnd, Duration: 4 * time.Minute})
	assertSignals(t, "under SlowTurnMin, never fires", got, nil)

	got = d.Observe(Event{Time: t0.Add(22 * time.Minute), Instance: inst, Thread: "t2", Kind: KindTurnEnd, Duration: 6 * time.Minute})
	assertSignals(t, "over SlowTurnMin and over the tiny rolling p90", got, []wantSig{{Code: CodeSlowTurn}})
}

// --- expensive_turn ---

func TestExpensiveTurnNoBaselineNeverFires(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)

	got := d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindTurnEnd, Tokens: Usage{InputTokens: 100000}})
	assertSignals(t, "no baseline yet, even a huge turn is quiet", got, nil)
}

func TestExpensiveTurnUsesRollingP95(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"

	for i := 0; i < 20; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(i) * time.Minute), Instance: inst, Thread: "base", Kind: KindTurnEnd, Tokens: Usage{InputTokens: 100}})
		assertSignals(t, "baseline sample", got, nil)
	}

	got := d.Observe(Event{Time: t0.Add(21 * time.Minute), Instance: inst, Thread: "t1", Kind: KindTurnEnd, Tokens: Usage{InputTokens: 100000}})
	assertSignals(t, "far over the rolling p95", got, []wantSig{{Code: CodeExpensiveTurn}})

	got = d.Observe(Event{Time: t0.Add(22 * time.Minute), Instance: inst, Thread: "t2", Kind: KindTurnEnd, Tokens: Usage{InputTokens: 100}})
	assertSignals(t, "normal turn stays quiet", got, nil)
}

// --- recovered_errors ---

func TestRecoveredErrors(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindToolError, Tool: "bash", Excerpt: "flaky"})
	got := d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "turn recovered from an error", got, []wantSig{{Code: CodeRecoveredErrors}})

	got = d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "clean turn: no recovered_errors", got, nil)
}

// --- compaction_churn ---

func TestCompactionChurn(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	for i := 0; i < 2; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(i) * time.Hour), Instance: inst, Thread: thr, Kind: KindCompaction})
		assertSignals(t, "under threshold", got, nil)
	}
	got := d.Observe(Event{Time: t0.Add(2 * time.Hour), Instance: inst, Thread: thr, Kind: KindCompaction})
	assertSignals(t, "3rd compaction in 24h triggers", got, []wantSig{{Code: CodeCompactionChurn}})

	got = d.Observe(Event{Time: t0.Add(3 * time.Hour), Instance: inst, Thread: thr, Kind: KindCompaction})
	assertSignals(t, "same day, no duplicate", got, nil)
}

// --- retry_thrash ---

func TestRetryThrash(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"
	excerpt := "TypeError: cannot read x"

	got := d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindToolError, Tool: "test", Excerpt: excerpt})
	assertSignals(t, "1st occurrence", got, nil)
	got = d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "turn ends (recovers)", got, []wantSig{{Code: CodeRecoveredErrors}})

	got = d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Thread: thr, Kind: KindToolError, Tool: "test", Excerpt: excerpt})
	assertSignals(t, "2nd occurrence, in a new turn", got, nil)
	got = d.Observe(Event{Time: t0.Add(3 * time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "turn ends again (recovers)", got, []wantSig{{Code: CodeRecoveredErrors}})

	got = d.Observe(Event{Time: t0.Add(4 * time.Minute), Instance: inst, Thread: thr, Kind: KindToolError, Tool: "test", Excerpt: excerpt})
	assertSignals(t, "3rd occurrence within the hour triggers retry_thrash", got, []wantSig{{Code: CodeRetryThrash}})

	got = d.Observe(Event{Time: t0.Add(5 * time.Minute), Instance: inst, Thread: thr, Kind: KindToolError, Tool: "test", Excerpt: excerpt})
	assertSignals(t, "4th occurrence, no duplicate", got, nil)
}

// --- config gating ---

func TestDisabledInstanceEmitsNothing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Instances = map[string]InstanceConf{"i1": {Disabled: true}}
	d := NewDetector(cfg)
	t0 := day(0)

	got := d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindAuthError})
	assertSignals(t, "observe on disabled instance", got, nil)

	got = d.Tick(t0.Add(time.Hour), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "tick on disabled instance", got, nil)

	if len(d.threads["i1"]) != 0 {
		t.Errorf("disabled instance should not accumulate thread state")
	}
}

// --- memory bounds ---

func TestTickEvictsIdleThreads(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindUserMessage})

	if _, ok := d.threads["i1"]["t1"]; !ok {
		t.Fatalf("expected thread state to exist before eviction")
	}
	d.Tick(t0.Add(49*time.Hour), []Instance{{Name: "i1", Status: "idle"}})
	if _, ok := d.threads["i1"]["t1"]; ok {
		t.Errorf("expected thread idle > 48h to be evicted")
	}
}

// --- Stats ---

func TestStats(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: "t1", Kind: KindAPIError})
	d.Observe(Event{Time: t0.Add(time.Second), Instance: inst, Thread: "t1", Kind: KindToolError, Tool: "bash"})
	d.Observe(Event{Time: t0.Add(2 * time.Second), Instance: inst, Thread: "t1", Kind: KindCompaction})
	d.Observe(Event{Time: t0.Add(3 * time.Second), Instance: inst, Thread: "t1", Kind: KindTurnEnd,
		Duration: 2 * time.Minute, Tokens: Usage{InputTokens: 500, OutputTokens: 100}})
	d.Observe(Event{Time: t0.Add(4 * time.Second), Instance: inst, Thread: "t1", Kind: KindTurnEnd,
		Duration: 4 * time.Minute, Tokens: Usage{InputTokens: 1000}})

	stats := d.Stats(inst)
	if stats.Turns != 2 {
		t.Errorf("Turns = %d, want 2", stats.Turns)
	}
	if stats.APIErrors != 1 {
		t.Errorf("APIErrors = %d, want 1", stats.APIErrors)
	}
	if stats.ToolErrors != 1 {
		t.Errorf("ToolErrors = %d, want 1", stats.ToolErrors)
	}
	if stats.Compactions != 1 {
		t.Errorf("Compactions = %d, want 1", stats.Compactions)
	}
	if stats.TotalTokens != 1600 {
		t.Errorf("TotalTokens = %d, want 1600", stats.TotalTokens)
	}
	if stats.P50TurnDuration == 0 || stats.P90TurnDuration == 0 {
		t.Errorf("expected non-zero turn duration percentiles, got p50=%s p90=%s", stats.P50TurnDuration, stats.P90TurnDuration)
	}

	if got := d.Stats("unknown"); got != (InstanceStats{}) {
		t.Errorf("Stats for unobserved instance = %+v, want zero value", got)
	}
}
