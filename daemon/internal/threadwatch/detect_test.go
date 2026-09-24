package threadwatch

import (
	"fmt"
	"strings"
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
	case CodeAwaitingUser, CodeErrorLoop, CodeAuthFailed, CodeStalledTurn, CodeDiedMidTurn, CodeUsageLimit:
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

	// The post-turn-end awaiting window is cleared by the activity (the
	// agent kept going on its own): ticking soon after produces nothing,
	// since it's neither past the old anchor nor past the open-turn
	// idle-prompt threshold measured from the activity itself.
	got = d.Tick(t0.Add(5*time.Minute), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "activity cleared the old awaiting window, not yet idle long enough to look like a new prompt", got, nil)

	// But the activity reopened the turn (threadState.open), and nothing
	// followed it. A turn that opens and then goes silent while the
	// instance sits idle is exactly the open-turn idle-prompt case
	// (detect.go's Tick, the fix for problem 2), so it now fires there.
	got = d.Tick(t0.Add(20*time.Minute), []Instance{{Name: "i1", Status: "idle"}})
	assertSignals(t, "no further activity after the reopen: looks like a stuck prompt", got, []wantSig{{Code: CodeAwaitingUser}})
}

// --- awaiting_user (open-turn idle prompt) ---

func TestOpenTurnIdlePromptFiresOnceThenResolvesOnActivity(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	instances := []Instance{{Name: "i1", Status: "idle"}}

	// A permission prompt/menu blocks the agent mid-turn: the turn opens
	// but never closes (no turn_end, no assistant_msg), so the ordinary
	// awaitingSince-anchored path never engages.
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindActivity})

	got := d.Tick(t0.Add(9*time.Minute), instances)
	assertSignals(t, "under threshold", got, nil)

	got = d.Tick(t0.Add(11*time.Minute), instances)
	assertSignals(t, "open turn idle past the threshold fires once", got, []wantSig{{Code: CodeAwaitingUser}})
	if !strings.Contains(got[0].Reason, "turn open") {
		t.Errorf("expected reason to describe an open turn, got %q", got[0].Reason)
	}

	got = d.Tick(t0.Add(12*time.Minute), instances)
	assertSignals(t, "no duplicate on the next tick", got, nil)

	// Any later event on the thread resolves it, even a non-user_message
	// one — the operator (or the agent on its own) moved past whatever it
	// was sitting on.
	got = d.Observe(Event{Time: t0.Add(15 * time.Minute), Instance: "i1", Thread: "t1", Kind: KindActivity})
	assertSignals(t, "resolves on any later event", got, []wantSig{{Code: CodeAwaitingUser, Resolved: true}})

	// And it can fire again after a fresh idle gap.
	got = d.Tick(t0.Add(26*time.Minute), instances)
	assertSignals(t, "fires again after a fresh idle gap", got, []wantSig{{Code: CodeAwaitingUser}})
}

func TestOpenTurnIdlePromptOnlyMostRecentThread(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	instances := []Instance{{Name: "i1", Status: "idle"}}

	// Two open turns on the same instance; t1 is older, t2 is the more
	// recently active one.
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindActivity})
	d.Observe(Event{Time: t0.Add(time.Minute), Instance: "i1", Thread: "t2", Kind: KindActivity})

	got := d.Tick(t0.Add(15*time.Minute), instances)
	assertSignals(t, "only the most recently active open thread fires", got, []wantSig{{Code: CodeAwaitingUser}})
	if got[0].Thread != "t2" {
		t.Errorf("expected the most-recently-active thread (t2) to fire, got %q", got[0].Thread)
	}

	// t1 never gets its turn this cycle or later — it's simply not the
	// most-recently-active thread — but nothing else should misfire either.
	got = d.Tick(t0.Add(16*time.Minute), instances)
	assertSignals(t, "no duplicate, and t1 still does not fire", got, nil)
}

func TestOpenTurnIdlePromptSkipsStaleThread(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	instances := []Instance{{Name: "i1", Status: "idle"}}

	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindActivity})

	// Well past both the awaiting-user threshold and the 6h stale cutoff:
	// this is an abandoned open turn, not a live prompt, so it must not
	// fire.
	got := d.Tick(t0.Add(7*time.Hour), instances)
	assertSignals(t, "stale (>6h) open turn never fires", got, nil)
}

func TestOpenTurnIdlePromptDoesNotDoubleFireWithPostTurnPath(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	instances := []Instance{{Name: "i1", Status: "idle"}}

	// A turn that actually ended (or produced a final assistant message)
	// is covered by the ordinary awaitingSince-anchored path; the open-turn
	// variant must stay out of its way and not also fire.
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindTurnEnd, Excerpt: "done, want me to continue?"})

	got := d.Tick(t0.Add(11*time.Minute), instances)
	assertSignals(t, "only the ordinary awaiting_user path fires, no duplicate from the open-turn path", got, []wantSig{{Code: CodeAwaitingUser}})
}

func TestOpenTurnIdlePromptRequiresIdleStatus(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	d.Observe(Event{Time: t0, Instance: "i1", Thread: "t1", Kind: KindActivity})

	// A running instance with an open, silent turn is stalled_turn's job,
	// not the open-turn awaiting_user variant's.
	got := d.Tick(t0.Add(20*time.Minute), []Instance{{Name: "i1", Status: "running"}})
	for _, s := range got {
		if s.Code == CodeAwaitingUser {
			t.Fatalf("open-turn awaiting_user must require idle status, got %+v", got)
		}
	}
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
	// stalled_turn itself never fires on an idle instance (it requires
	// status "running"). But this is exactly the open-turn idle-prompt
	// scenario the problem-2 fix targets: an open turn, idle status, no
	// event for AwaitingUserAfter — so awaiting_user fires instead, via the
	// open-turn path in Tick.
	for _, s := range got {
		if s.Code == CodeStalledTurn {
			t.Fatalf("stalled_turn must never fire on an idle instance, got %+v", got)
		}
	}
	assertSignals(t, "idle status with an open turn raises the open-turn awaiting_user instead", got, []wantSig{{Code: CodeAwaitingUser}})
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

// --- usage_limit ---

func TestUsageLimitBlocksAwaitingUserAndStalledTurn(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUserMessage})
	got := d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: thr, Kind: KindUsageLimit,
		Excerpt: "You've hit your session limit · resets 7am (UTC)"})
	assertSignals(t, "usage limit fires once", got, []wantSig{{Code: CodeUsageLimit}})
	if !strings.Contains(got[0].Reason, "resets 7am (UTC)") {
		t.Errorf("expected the reset phrase in the reason, got %q", got[0].Reason)
	}

	// Claude Code emits a turn_duration record immediately after an
	// isApiErrorMessage one, even for an interrupted turn: that turn_end is
	// the tail of the same broken turn, not recovery, so it must not clear
	// the block.
	got = d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "the immediate turn_end tail does not clear the block", got, nil)

	got = d.Tick(t0.Add(2*time.Hour), []Instance{{Name: inst, Status: "idle"}})
	assertSignals(t, "blocked thread: no awaiting_user while idle, however long", got, nil)

	got = d.Tick(t0.Add(3*time.Hour), []Instance{{Name: inst, Status: "running"}})
	assertSignals(t, "blocked thread: no stalled_turn while running, however long", got, nil)
}

func TestUsageLimitClearsOnUserMessageThenNormalDetectionResumes(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUsageLimit, Excerpt: "hit your usage limit, resets in 2 hours"})
	d.Observe(Event{Time: t0.Add(time.Second), Instance: inst, Thread: thr, Kind: KindTurnEnd}) // same broken turn's tail

	got := d.Observe(Event{Time: t0.Add(time.Hour), Instance: inst, Thread: thr, Kind: KindUserMessage})
	assertSignals(t, "a later user message clears the usage_limit block", got, []wantSig{{Code: CodeUsageLimit, Resolved: true}})

	d.Observe(Event{Time: t0.Add(time.Hour + time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd,
		Excerpt: "back to normal, should I continue?"})
	got = d.Tick(t0.Add(time.Hour+12*time.Minute), []Instance{{Name: inst, Status: "idle"}})
	assertSignals(t, "awaiting_user fires normally again once the block has cleared", got, []wantSig{{Code: CodeAwaitingUser}})
}

func TestUsageLimitClearsOnLaterActivityNotTheImmediateTail(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	got := d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUsageLimit, Excerpt: "usage limit reached"})
	assertSignals(t, "first episode pages", got, []wantSig{{Code: CodeUsageLimit}})

	got = d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: thr, Kind: KindActivity})
	assertSignals(t, "activity immediately following the limit is the tail of the same broken turn: stays blocked", got, nil)

	got = d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Thread: thr, Kind: KindActivity})
	assertSignals(t, "a later activity is real recovery", got, []wantSig{{Code: CodeUsageLimit, Resolved: true}})
}

func TestUsageLimitOnePerEpisodeNoDuplicateWhileBlocked(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	for i := 0; i < 5; i++ {
		got := d.Observe(Event{Time: t0.Add(time.Duration(i) * time.Second), Instance: inst, Thread: thr, Kind: KindUsageLimit, Excerpt: "usage limit reached"})
		if i == 0 {
			assertSignals(t, "first usage_limit event pages", got, []wantSig{{Code: CodeUsageLimit}})
		} else {
			assertSignals(t, "repeated usage_limit events within the same open episode: no duplicate", got, nil)
		}
	}

	got := d.Observe(Event{Time: t0.Add(10 * time.Second), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "the tail turn_end of the last hit: no recovered_errors, no error_loop (usage_limit never counts as an api_error)", got, nil)

	if stats := d.Stats(inst); stats.APIErrors != 0 {
		t.Errorf("APIErrors = %d, want 0: usage_limit must not count toward api_error stats", stats.APIErrors)
	}
}

func TestUsageLimitCooldownAcrossInstance(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst := "i1"

	got := d.Observe(Event{Time: t0, Instance: inst, Thread: "t1", Kind: KindUsageLimit, Excerpt: "usage limit reached"})
	assertSignals(t, "first episode pages", got, []wantSig{{Code: CodeUsageLimit}})

	// Force the episode "closed" without the thread having made progress,
	// simulating an episode that ended some way other than the detector's
	// own recovery paths (turn_end/activity/user_message).
	d.instances[inst].usageLimitActive = false

	got = d.Observe(Event{Time: t0.Add(time.Hour), Instance: inst, Thread: "t2", Kind: KindUsageLimit, Excerpt: "usage limit reached again"})
	assertSignals(t, "a second episode within the 6h cooldown, no progress in between: stays quiet", got, nil)

	// This second (suppressed) episode also "closes" without progress.
	d.instances[inst].usageLimitActive = false

	got = d.Observe(Event{Time: t0.Add(7 * time.Hour), Instance: inst, Thread: "t3", Kind: KindUsageLimit, Excerpt: "usage limit reached a third time"})
	assertSignals(t, "past the 6h cooldown: pages again", got, []wantSig{{Code: CodeUsageLimit}})
}

func TestUsageLimitProgressBypassesCooldown(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindUsageLimit, Excerpt: "usage limit reached"})
	d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: thr, Kind: KindActivity})     // tail, stays blocked
	d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Thread: thr, Kind: KindActivity}) // real recovery: progressed=true

	// A new episode well within the 6h cooldown, but after progress: pages
	// immediately rather than waiting out the cooldown.
	got := d.Observe(Event{Time: t0.Add(10 * time.Minute), Instance: inst, Thread: thr, Kind: KindUsageLimit, Excerpt: "usage limit reached again"})
	assertSignals(t, "new episode after progress bypasses the cooldown", got, []wantSig{{Code: CodeUsageLimit}})
}

// --- awaiting_user evidence freshness (fix: stale/absent evidence) ---

func TestTurnEndAfterAPIErrorWithNoFreshMessage_NoAwaitingUserAnchor(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	// A much earlier turn ended with a real question; this must never leak
	// into evidence for a later, unrelated turn that gets cut short by an
	// API error with no fresh assistant message of its own.
	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindTurnEnd, Excerpt: "should I proceed with the migration?"})
	d.Observe(Event{Time: t0.Add(time.Minute), Instance: inst, Thread: thr, Kind: KindUserMessage}) // new turn starts, clears stale evidence
	d.Observe(Event{Time: t0.Add(2 * time.Minute), Instance: inst, Thread: thr, Kind: KindAPIError, Excerpt: "Prompt is too long"})
	got := d.Observe(Event{Time: t0.Add(3 * time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})
	assertSignals(t, "turn_end right after an api_error: recovered_errors insight only, no awaiting_user anchor",
		got, []wantSig{{Code: CodeRecoveredErrors}})

	got = d.Tick(t0.Add(20*time.Minute), []Instance{{Name: inst, Status: "idle"}})
	assertSignals(t, "no stale evidence anchored an awaiting_user window", got, nil)
}

func TestUserMessageResetsStaleAssistantExcerpt(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	t0 := day(0)
	inst, thr := "i1", "t1"

	d.Observe(Event{Time: t0, Instance: inst, Thread: thr, Kind: KindAssistantMsg, Excerpt: "should I delete the branch?"})
	got := d.Tick(t0.Add(11*time.Minute), []Instance{{Name: inst, Status: "idle"}})
	assertSignals(t, "fires with the real question as evidence", got, []wantSig{{Code: CodeAwaitingUser}})
	if got[0].Evidence != "should I delete the branch?" {
		t.Fatalf("evidence = %q, want the real question", got[0].Evidence)
	}

	// The operator replies; a later turn hits an API error with no
	// assistant text before ending.
	d.Observe(Event{Time: t0.Add(12 * time.Minute), Instance: inst, Thread: thr, Kind: KindUserMessage})
	d.Observe(Event{Time: t0.Add(13 * time.Minute), Instance: inst, Thread: thr, Kind: KindAPIError, Excerpt: "rate limited"})
	d.Observe(Event{Time: t0.Add(14 * time.Minute), Instance: inst, Thread: thr, Kind: KindTurnEnd})

	got = d.Tick(t0.Add(30*time.Minute), []Instance{{Name: inst, Status: "idle"}})
	assertSignals(t, "no stale question reused as evidence for the unrelated interrupted turn", got, nil)
}
