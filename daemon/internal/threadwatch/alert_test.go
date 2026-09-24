package threadwatch

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic dedup/rate-limit
// tests.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// fakeSender records every message it was asked to send, and can be made
// to fail on demand.
type fakeSender struct {
	mu       sync.Mutex
	messages []string
	failNext bool
	failAll  bool
}

func (s *fakeSender) send(message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll || s.failNext {
		s.failNext = false
		return fmt.Errorf("boom")
	}
	s.messages = append(s.messages, message)
	return nil
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

func (s *fakeSender) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return ""
	}
	return s.messages[len(s.messages)-1]
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Alerts = AlertConfig{Cooldown: time.Hour, MaxPerHour: 2, ResolvedAfter: time.Hour}
	cfg.Jev = JevConfig{Mode: "shadow", PageUrgency: 4, PageConfidence: 0.6, AwaitingMinProb: 0.5}
	return cfg
}

func baseSignal(code string) Signal {
	return Signal{
		Instance: "myinst",
		Thread:   "abcdef1234567890",
		Code:     code,
		Tier:     TierIntervene,
		Reason:   "test reason",
		Evidence: "some evidence",
	}
}

func TestHandle_InsightNeverPages(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	a := NewAlerter(testConfig(), "myhost", sender.send, clock.now)

	sig := baseSignal(CodeSlowTurn)
	sig.Tier = TierInsight
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("insight tier must never page, got %+v", d)
	}
	if d.Reason != "insight" {
		t.Fatalf("want reason %q, got %q", "insight", d.Reason)
	}
	if sender.count() != 0 {
		t.Fatalf("expected no messages sent, got %d", sender.count())
	}
}

func TestHandle_LiveJevGate_AwaitingUser_Rejects(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeAwaitingUser)
	sig.Judgment = &Judgment{
		NeedsHumanNow: 0.9,
		WaitingKind:   "finished", // excluded kind, even though prob is high
		Urgency:       5,
		UrgencyConf:   0.9,
	}
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected no page when waiting_kind is finished, got %+v", d)
	}
	if !strings.HasPrefix(d.Reason, "jev: ") {
		t.Fatalf("expected jev-prefixed reason, got %q", d.Reason)
	}
	if sender.count() != 0 {
		t.Fatalf("expected no messages sent, got %d", sender.count())
	}
}

func TestHandle_LiveJevGate_AwaitingUser_LowProbRejects(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeAwaitingUser)
	sig.Judgment = &Judgment{
		NeedsHumanNow: 0.1, // below AwaitingMinProb
		WaitingKind:   "question",
		Urgency:       5,
		UrgencyConf:   0.9,
	}
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected no page when needs_human_now is low, got %+v", d)
	}
	if sender.count() != 0 {
		t.Fatalf("expected no messages sent, got %d", sender.count())
	}
}

func TestHandle_LiveJevGate_AwaitingUser_Accepts(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeAwaitingUser)
	sig.Judgment = &Judgment{
		NeedsHumanNow: 0.9,
		WaitingKind:   "question",
		Urgency:       5,
		UrgencyConf:   0.9,
	}
	d := a.Handle(sig)

	if !d.Page {
		t.Fatalf("expected a page, got %+v", d)
	}
	if sender.count() != 1 {
		t.Fatalf("expected one message sent, got %d", sender.count())
	}
}

// A session waiting on the operator pages on its own rule; a low urgency
// score must not suppress it.
func TestHandle_LiveJevGate_AwaitingUser_IgnoresUrgency(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeAwaitingUser)
	sig.Judgment = &Judgment{NeedsHumanNow: 0.8, WaitingKind: "question_to_user", Urgency: 2, UrgencyConf: 0.9}
	if d := a.Handle(sig); !d.Page {
		t.Fatalf("expected a page despite low urgency, got %+v", d)
	}
}

func TestHandle_LiveJevGate_GeneralCode_UrgencyTooLow(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeErrorLoop)
	sig.Judgment = &Judgment{Urgency: 2.1, UrgencyConf: 0.9}
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected no page when urgency below threshold, got %+v", d)
	}
	if !strings.Contains(d.Reason, "jev:") {
		t.Fatalf("expected jev reason, got %q", d.Reason)
	}
}

func TestHandle_LiveJevGate_AuthFailedAndDiedMidTurn_AlwaysPage(t *testing.T) {
	for _, code := range []string{CodeAuthFailed, CodeDiedMidTurn} {
		clock := &fakeClock{t: time.Now()}
		sender := &fakeSender{}
		cfg := testConfig()
		cfg.Jev.Mode = "live"
		a := NewAlerter(cfg, "myhost", sender.send, clock.now)

		sig := baseSignal(code)
		sig.Judgment = &Judgment{Urgency: 1, UrgencyConf: 0.01} // terrible scores
		d := a.Handle(sig)

		if !d.Page {
			t.Fatalf("%s: expected always-page exemption to page, got %+v", code, d)
		}
	}
}

func TestHandle_ShadowMode_BypassesButRecordsWouldSuppress(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "shadow"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeErrorLoop)
	sig.Judgment = &Judgment{Urgency: 2.1, UrgencyConf: 0.9} // would fail live gate
	d := a.Handle(sig)

	if !d.Page {
		t.Fatalf("expected shadow mode to fail open and page, got %+v", d)
	}
	if !strings.Contains(d.Reason, "shadow: would suppress") {
		t.Fatalf("expected shadow note in reason, got %q", d.Reason)
	}
	if sender.count() != 1 {
		t.Fatalf("expected one message sent, got %d", sender.count())
	}
}

func TestHandle_OffMode_BypassesAndPagesNormally(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeErrorLoop)
	sig.Judgment = &Judgment{Urgency: 1, UrgencyConf: 0.9}
	d := a.Handle(sig)

	if !d.Page {
		t.Fatalf("expected off mode to fail open and page, got %+v", d)
	}
}

func TestHandle_NilOrErroredJudgment_BypassesGate(t *testing.T) {
	cfg := testConfig()
	cfg.Jev.Mode = "live"

	t.Run("nil judgment", func(t *testing.T) {
		clock := &fakeClock{t: time.Now()}
		sender := &fakeSender{}
		a := NewAlerter(cfg, "myhost", sender.send, clock.now)
		sig := baseSignal(CodeErrorLoop) // Judgment left nil
		d := a.Handle(sig)
		if !d.Page {
			t.Fatalf("expected nil judgment to fail open and page, got %+v", d)
		}
		if strings.Contains(d.Reason, "shadow:") {
			t.Fatalf("no verdict was available, should not claim what live mode would do: %q", d.Reason)
		}
	})

	t.Run("errored judgment", func(t *testing.T) {
		clock := &fakeClock{t: time.Now()}
		sender := &fakeSender{}
		a := NewAlerter(cfg, "myhost", sender.send, clock.now)
		sig := baseSignal(CodeErrorLoop)
		sig.Judgment = &Judgment{Urgency: 1, UrgencyConf: 0.9, Err: "timeout"}
		d := a.Handle(sig)
		if !d.Page {
			t.Fatalf("expected errored judgment to fail open and page, got %+v", d)
		}
	})
}

func TestHandle_Dedup(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeStalledTurn)
	d1 := a.Handle(sig)
	if !d1.Page {
		t.Fatalf("first occurrence should page, got %+v", d1)
	}

	clock.advance(10 * time.Minute)
	d2 := a.Handle(sig)
	if d2.Page {
		t.Fatalf("repeat within cooldown should not page, got %+v", d2)
	}
	if !strings.HasPrefix(d2.Reason, "dedup:") {
		t.Fatalf("expected dedup reason, got %q", d2.Reason)
	}
	if sender.count() != 1 {
		t.Fatalf("expected exactly one message sent so far, got %d", sender.count())
	}

	// Past the cooldown, it should page again.
	clock.advance(time.Hour)
	d3 := a.Handle(sig)
	if !d3.Page {
		t.Fatalf("expected page after cooldown elapsed, got %+v", d3)
	}
	if sender.count() != 2 {
		t.Fatalf("expected two messages sent, got %d", sender.count())
	}
}

func TestHandle_DedupKeyIncludesInstanceThreadCode(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeStalledTurn)
	a.Handle(sig)

	other := sig
	other.Thread = "different-thread-id"
	d := a.Handle(other)
	if !d.Page {
		t.Fatalf("a different thread should not be deduped against, got %+v", d)
	}
}

func TestHandle_RateLimit(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	cfg.Alerts.MaxPerHour = 2
	cfg.Alerts.Cooldown = 0 // isolate rate limiting from dedup
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	// Three distinct keys so dedup doesn't interfere.
	sig1 := baseSignal(CodeStalledTurn)
	sig1.Thread = "thread-one"
	sig2 := baseSignal(CodeStalledTurn)
	sig2.Thread = "thread-two"
	sig3 := baseSignal(CodeStalledTurn)
	sig3.Thread = "thread-three"

	d1 := a.Handle(sig1)
	d2 := a.Handle(sig2)
	if !d1.Page || !d2.Page {
		t.Fatalf("first two pages within the cap should send, got %+v %+v", d1, d2)
	}
	if sender.count() != 2 {
		t.Fatalf("expected two messages sent, got %d", sender.count())
	}

	d3 := a.Handle(sig3)
	if d3.Page {
		t.Fatalf("third page should be held by the rate limit, got %+v", d3)
	}
	if !strings.HasPrefix(d3.Reason, "rate_limited:") {
		t.Fatalf("expected rate_limited reason, got %q", d3.Reason)
	}
	if d3.Message == "" {
		t.Fatalf("expected a formatted message to still be recorded for logging")
	}
	if sender.count() != 2 {
		t.Fatalf("held alert must not be sent, got %d messages", sender.count())
	}

	// Roll the window forward so capacity frees up; the next send should
	// carry the "N more held" note.
	clock.advance(61 * time.Minute)
	sig4 := baseSignal(CodeStalledTurn)
	sig4.Thread = "thread-four"
	d4 := a.Handle(sig4)
	if !d4.Page {
		t.Fatalf("expected page once window rolled over, got %+v", d4)
	}
	if !strings.Contains(sender.last(), "more alert") {
		t.Fatalf("expected held-count note in next sent message, got %q", sender.last())
	}

	// The held counter should have been reset — a further hold-worthy
	// signal starts counting from zero again. The rolled-over window only
	// has sig4 in it so far (cap 2), so one more send is still allowed
	// before the cap bites again.
	sig5 := baseSignal(CodeStalledTurn)
	sig5.Thread = "thread-five"
	d5 := a.Handle(sig5)
	if !d5.Page {
		t.Fatalf("expected window to still have capacity for a second send, got %+v", d5)
	}

	sig6 := baseSignal(CodeStalledTurn)
	sig6.Thread = "thread-six"
	d6 := a.Handle(sig6)
	if d6.Page {
		t.Fatalf("expected the next page to be held again (cap reached), got %+v", d6)
	}
	if !strings.Contains(d6.Reason, "1 total held") {
		t.Fatalf("expected held counter to have reset to 1, got %q", d6.Reason)
	}
}

func TestFlush_SendsRollupWhenHeld(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	cfg.Alerts.MaxPerHour = 0 // force every page to hold via explicit test below
	cfg.Alerts.Cooldown = 0
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	if err := a.Flush(); err != nil {
		t.Fatalf("flush with nothing held should be a no-op, got err %v", err)
	}
	if sender.count() != 0 {
		t.Fatalf("expected no messages sent by a no-op flush, got %d", sender.count())
	}

	// Force a hold directly by driving MaxPerHour down after construction.
	a.cfg.Alerts.MaxPerHour = 1
	sig1 := baseSignal(CodeStalledTurn)
	sig1.Thread = "t1"
	sig2 := baseSignal(CodeStalledTurn)
	sig2.Thread = "t2"
	a.Handle(sig1)
	d2 := a.Handle(sig2)
	if d2.Page {
		t.Fatalf("expected second page to be held, got %+v", d2)
	}

	if err := a.Flush(); err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	if sender.count() != 2 {
		t.Fatalf("expected flush to send a rollup message, got %d total messages", sender.count())
	}
	if !strings.Contains(sender.last(), "held") {
		t.Fatalf("expected rollup message to mention held alerts, got %q", sender.last())
	}

	// A second flush with nothing held should do nothing further.
	if err := a.Flush(); err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	if sender.count() != 2 {
		t.Fatalf("expected no further message from an empty flush, got %d", sender.count())
	}
}

func TestHandle_Resolved_WithinWindow_Sends(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeStalledTurn)
	a.Handle(sig) // pages

	clock.advance(30 * time.Minute) // within ResolvedAfter (1h)
	resolved := sig
	resolved.Resolved = true
	d := a.Handle(resolved)

	if !d.Page {
		t.Fatalf("expected resolved notice to send within the window, got %+v", d)
	}
	if !strings.Contains(d.Message, "resolved") {
		t.Fatalf("expected resolved wording in message, got %q", d.Message)
	}
	if sender.count() != 2 {
		t.Fatalf("expected two messages sent (page + resolved), got %d", sender.count())
	}
}

func TestHandle_Resolved_OutsideWindow_SendsNothing(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeStalledTurn)
	a.Handle(sig) // pages

	clock.advance(2 * time.Hour) // outside ResolvedAfter (1h)
	resolved := sig
	resolved.Resolved = true
	d := a.Handle(resolved)

	if d.Page {
		t.Fatalf("expected no resolved notice outside the window, got %+v", d)
	}
	if sender.count() != 1 {
		t.Fatalf("expected only the original page to have sent, got %d", sender.count())
	}
}

// --- FIX 1: no "resolved" notice for awaiting_user/usage_limit ---

func TestHandleResolved_AwaitingUser_NoNoticeButClearsDedup(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("I've finished the migration. Should I also update the docs?")
	d1 := a.Handle(sig)
	if !d1.Page {
		t.Fatalf("expected the initial page to send, got %+v", d1)
	}

	clock.advance(time.Minute) // well within ResolvedAfter/Cooldown
	resolved := sig
	resolved.Resolved = true
	d2 := a.Handle(resolved)
	if d2.Page {
		t.Fatalf("expected no notice for a resolved awaiting_user (the operator resolving it themselves isn't news), got %+v", d2)
	}
	if sender.count() != 1 {
		t.Fatalf("expected no message sent for the resolved notice, got %d total messages", sender.count())
	}

	// Dedup must still have cleared: a fresh wait pages immediately, even
	// though we're well inside the cooldown window.
	d3 := a.Handle(sig)
	if !d3.Page {
		t.Fatalf("expected dedup to have cleared so a new wait pages immediately, got %+v", d3)
	}
	if sender.count() != 2 {
		t.Fatalf("expected two total messages sent, got %d", sender.count())
	}
}

func TestHandleResolved_UsageLimit_NoNoticeButClearsDedup(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeUsageLimit)
	d1 := a.Handle(sig)
	if !d1.Page {
		t.Fatalf("expected the initial page to send, got %+v", d1)
	}

	clock.advance(time.Minute)
	resolved := sig
	resolved.Resolved = true
	d2 := a.Handle(resolved)
	if d2.Page {
		t.Fatalf("expected no notice for a resolved usage_limit (it clears on its own), got %+v", d2)
	}
	if sender.count() != 1 {
		t.Fatalf("expected no message sent for the resolved notice, got %d total messages", sender.count())
	}

	d3 := a.Handle(sig)
	if !d3.Page {
		t.Fatalf("expected dedup to have cleared so a new episode pages immediately, got %+v", d3)
	}
}

// Codes that CAN clear without the operator still get their resolved
// notice: it's news (the session fixed itself).
func TestHandleResolved_OtherCodesStillNotify(t *testing.T) {
	for _, code := range []string{CodeErrorLoop, CodeAuthFailed, CodeStalledTurn, CodeDiedMidTurn} {
		t.Run(code, func(t *testing.T) {
			clock := &fakeClock{t: time.Now()}
			sender := &fakeSender{}
			cfg := testConfig()
			cfg.Jev.Mode = "off"
			a := NewAlerter(cfg, "myhost", sender.send, clock.now)

			sig := baseSignal(code)
			a.Handle(sig)

			clock.advance(time.Minute)
			resolved := sig
			resolved.Resolved = true
			d := a.Handle(resolved)
			if !d.Page {
				t.Fatalf("%s: expected a resolved notice to send, got %+v", code, d)
			}
			if sender.count() != 2 {
				t.Fatalf("%s: expected two messages sent (page + resolved), got %d", code, sender.count())
			}
		})
	}
}

// --- awaiting_user gate: usage_limit/error_blocked waiting kinds ---

func TestHandle_LiveJevGate_AwaitingUser_UsageLimitAndErrorBlockedRejects(t *testing.T) {
	for _, wk := range []string{"usage_limit", "error_blocked"} {
		t.Run(wk, func(t *testing.T) {
			clock := &fakeClock{t: time.Now()}
			sender := &fakeSender{}
			cfg := testConfig()
			cfg.Jev.Mode = "live"
			a := NewAlerter(cfg, "myhost", sender.send, clock.now)

			sig := baseSignal(CodeAwaitingUser)
			sig.Judgment = &Judgment{NeedsHumanNow: 0.99, WaitingKind: wk, Urgency: 5, UrgencyConf: 0.9}
			d := a.Handle(sig)

			if d.Page {
				t.Fatalf("expected waiting_kind=%s to be excluded from paging as awaiting_user (a dedicated signal covers it), got %+v", wk, d)
			}
			if sender.count() != 0 {
				t.Fatalf("expected no message sent, got %d", sender.count())
			}
		})
	}
}

func TestHandle_Resolved_WithoutPriorPage_SendsNothing(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	a := NewAlerter(testConfig(), "myhost", sender.send, clock.now)

	sig := baseSignal(CodeStalledTurn)
	sig.Resolved = true
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected no resolved notice without a prior page, got %+v", d)
	}
	if sender.count() != 0 {
		t.Fatalf("expected no messages sent, got %d", sender.count())
	}
}

func TestHandle_SenderError_DoesNotMarkPaged_AndRetries(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{failNext: true}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := baseSignal(CodeStalledTurn)
	d1 := a.Handle(sig)
	if d1.Page {
		t.Fatalf("expected Page=false when the sender errors, got %+v", d1)
	}
	if !strings.Contains(d1.Reason, "send error") {
		t.Fatalf("expected send error in reason, got %q", d1.Reason)
	}
	if d1.Message == "" {
		t.Fatalf("expected the formatted message to still be recorded")
	}

	// No cooldown should have been recorded, so it retries immediately.
	d2 := a.Handle(sig)
	if !d2.Page {
		t.Fatalf("expected retry to succeed and page, got %+v", d2)
	}
	if sender.count() != 1 {
		t.Fatalf("expected exactly one successful send, got %d", sender.count())
	}
}

func TestHandle_ConcurrencySafe(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	cfg.Alerts.MaxPerHour = 1000
	cfg.Alerts.Cooldown = 0
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sig := baseSignal(CodeStalledTurn)
			sig.Thread = fmt.Sprintf("thread-%d", i)
			a.Handle(sig)
		}(i)
	}
	wg.Wait()
	_ = a.Flush()

	if sender.count() != 50 {
		t.Fatalf("expected all 50 distinct signals to page, got %d", sender.count())
	}
}

// --- looksLikeWaiting ---

func TestLooksLikeWaiting(t *testing.T) {
	cases := []struct {
		name     string
		evidence string
		want     bool
	}{
		{"plain question", "I've reviewed the code. Should I also update the tests?", true},
		{"question with trailing quote", `The agent said "should I proceed?"`, true},
		{"question with trailing markdown", "**Should I continue?**", true},
		{"question with closing paren", "Should I go ahead and delete the branch (origin/old-feature)?", true},
		{"do you want", "Do you want me to open a PR for this?", true},
		{"would you like", "I found three matches. Would you like me to pick one.", true},
		{"shall i", "Shall I proceed with the migration.", true},
		{"should i case insensitive", "SHOULD I retry the failed step.", true},
		{"which option", "Which option do you prefer, A or B.", true},
		{"approve", "Please approve this plan before I continue.", true},
		{"permission", "I need permission to run this command.", true},
		{"y/n parens", "Continue? (y/n)", true},
		{"y/n brackets", "Overwrite the file [y/n]", true},
		{"press enter", "Press enter to continue, or Ctrl-C to cancel.", true},
		{"waiting for your", "Waiting for your input before proceeding.", true},
		// A finished summary's sign-off is not a question.
		{"let me know sign-off", "All tests pass and the branch is pushed. Let me know if you want me to go further.", false},
		{"permission in prose", "Fixed the file permission bug in the uploader; tests pass.", false},
		{"numbered menu cursor angle", "Select an option:\n❯ 1. Yes\n  2. No", true},
		{"numbered menu cursor gt", "Select an option:\n> 1. Yes\n  2. No", true},
		{"menu with yes cursor", "Do this?\n❯ Yes\n  No", true},

		{"plain done summary", "Ran the tests and fixed the failing case. Done.", false},
		{"plain done with trailing markdown", "All set. **Done.**", false},
		{"multi-sentence non-question tail", "Is that ok? No, I went ahead and finished it anyway.", false},
		{"empty evidence", "", false},
		{"whitespace only", "   \n\n  ", false},
		{"rhetorical-looking but no marker, ends in period", "I've completed the refactor across all packages.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeWaiting(c.evidence); got != c.want {
				t.Errorf("looksLikeWaiting(%q) = %v, want %v", c.evidence, got, c.want)
			}
		})
	}
}

func TestLooksLikeWaiting_OnlyInspectsTail(t *testing.T) {
	// A question far outside the last ~600 bytes must not count; padding
	// with plain non-question text keeps the tail itself unambiguous.
	padding := strings.Repeat("line of finished work with no question mark here.\n", 50)
	evidence := "Should I continue?\n" + padding + "All done, nothing further needed."
	if looksLikeWaiting(evidence) {
		t.Fatalf("expected the leading question outside the tail window to be ignored")
	}
}

// --- CodeAwaitingUser rule-vs-Jev gating ---

func awaitingSignalWithEvidence(evidence string) Signal {
	sig := baseSignal(CodeAwaitingUser)
	sig.Evidence = evidence
	return sig
}

func TestHandle_AwaitingUser_ShadowMode_RuleSuppresses(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "shadow"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("All done, no more changes needed.")
	// No Judgment scored (e.g. Jev wasn't reachable this cycle either).
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected the rule to suppress a non-question evidence tail, got %+v", d)
	}
	if d.Reason != "rule: no question or prompt in the last message" {
		t.Fatalf("unexpected reason: %q", d.Reason)
	}
	if sender.count() != 0 {
		t.Fatalf("expected no message sent, got %d", sender.count())
	}
}

func TestHandle_AwaitingUser_ShadowMode_RulePages(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "shadow"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("I've finished the migration. Should I also update the docs?")
	d := a.Handle(sig)

	if !d.Page {
		t.Fatalf("expected the rule to allow a real question through, got %+v", d)
	}
	if sender.count() != 1 {
		t.Fatalf("expected one message sent, got %d", sender.count())
	}
}

func TestHandle_AwaitingUser_OffMode_UsesRule(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("Finished up, everything looks good.")
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected off mode to still apply the rule (not fail open) for awaiting_user, got %+v", d)
	}
	if !strings.HasPrefix(d.Reason, "rule:") {
		t.Fatalf("expected rule-prefixed reason, got %q", d.Reason)
	}
}

func TestHandle_AwaitingUser_NilJudgment_UsesRule(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live" // even in live mode, a nil Judgment falls back to the rule
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("Wrapped up the task, no issues.")
	// sig.Judgment left nil.
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected nil judgment to fall back to the rule and suppress, got %+v", d)
	}
	if d.Reason != "rule: no question or prompt in the last message" {
		t.Fatalf("unexpected reason: %q", d.Reason)
	}
}

func TestHandle_AwaitingUser_ErroredJudgment_UsesRule(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("Should I go ahead and deploy this?")
	sig.Judgment = &Judgment{Err: "timeout", NeedsHumanNow: 0.99, WaitingKind: "question_to_user"}
	d := a.Handle(sig)

	if !d.Page {
		t.Fatalf("expected an errored judgment to fall back to the rule, which should page on a real question, got %+v", d)
	}
}

func TestHandle_AwaitingUser_LiveMode_JevDecidesAlone_IgnoresRule(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "live"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	// Evidence has no question or prompt marker (the rule would suppress),
	// but a valid live Judgment says the session needs the human now: live
	// mode's Jev gate decides alone, unchanged by this fix.
	sig := awaitingSignalWithEvidence("All finished, nothing more to do.")
	sig.Judgment = &Judgment{NeedsHumanNow: 0.9, WaitingKind: "question_to_user", Urgency: 5, UrgencyConf: 0.9}
	d := a.Handle(sig)

	if !d.Page {
		t.Fatalf("expected live mode's Jev verdict to page despite the rule disagreeing, got %+v", d)
	}
}

func TestHandle_AwaitingUser_ShadowMode_DisagreementNote_RuleSuppressesJevWaiting(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "shadow"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("Everything is done, thanks!")
	sig.Judgment = &Judgment{NeedsHumanNow: 0.85, WaitingKind: "question_to_user"}
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected the rule to suppress (no question in evidence), got %+v", d)
	}
	if !strings.Contains(d.Reason, "jev says waiting") {
		t.Fatalf("expected a disagreement note recording that jev says waiting, got %q", d.Reason)
	}
	if !strings.HasPrefix(d.Reason, "rule:") {
		t.Fatalf("expected the rule-prefixed reason to lead, got %q", d.Reason)
	}
}

func TestHandle_AwaitingUser_ShadowMode_AgreementNote_WouldSuppress(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "shadow"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("Everything is done, thanks!")
	sig.Judgment = &Judgment{NeedsHumanNow: 0.1, WaitingKind: "finished"}
	d := a.Handle(sig)

	if d.Page {
		t.Fatalf("expected the rule to suppress, got %+v", d)
	}
	if !strings.Contains(d.Reason, "shadow: would suppress") {
		t.Fatalf("expected the usual would-suppress note when rule and jev agree, got %q", d.Reason)
	}
}

func TestHandle_AwaitingUser_Retiers_ToInsight_ViaRulePrefix(t *testing.T) {
	// serve.go re-tiers a suppressed intervene signal to insight when the
	// Decision.Reason starts with "rule:" (as well as "jev:"); this just
	// pins down that Handle actually produces that prefix in the plain
	// suppress case, since serve_test.go exercises the re-tiering itself.
	clock := &fakeClock{t: time.Now()}
	sender := &fakeSender{}
	cfg := testConfig()
	cfg.Jev.Mode = "off"
	a := NewAlerter(cfg, "myhost", sender.send, clock.now)

	sig := awaitingSignalWithEvidence("No question here, just a status update.")
	d := a.Handle(sig)
	if strings.HasPrefix(d.Reason, "jev:") {
		t.Fatalf("did not expect a jev-prefixed reason here: %q", d.Reason)
	}
	if !strings.HasPrefix(d.Reason, "rule:") {
		t.Fatalf("expected a rule-prefixed reason, got %q", d.Reason)
	}
}

func TestFormatAlert(t *testing.T) {
	sig := Signal{
		Instance: "myinst",
		Thread:   "abcdef1234567890",
		Code:     CodeAwaitingUser,
		Tier:     TierIntervene,
		Reason:   "idle for 12m on a question, cc @everyone @here",
		Evidence: "line one\nline two",
		Judgment: &Judgment{WaitingKind: "question to user"},
	}
	msg := FormatAlert("myhost", sig)

	if !strings.HasPrefix(msg, "⏳ myinst · abcdef12 on myhost") {
		t.Fatalf("unexpected head line: %q", msg)
	}
	if !strings.Contains(msg, "Waiting: question to user") {
		t.Fatalf("expected waiting_kind line, got %q", msg)
	}
	if !strings.Contains(msg, "> line one") || !strings.Contains(msg, "> line two") {
		t.Fatalf("expected quoted evidence lines, got %q", msg)
	}
	if !strings.Contains(msg, "Attach: `agentmux` → select myinst → a") {
		t.Fatalf("expected attach line, got %q", msg)
	}
	if strings.Contains(msg, "@everyone") || strings.Contains(msg, "@here") {
		t.Fatalf("mentions were not neutralised: %q", msg)
	}
	if !strings.Contains(msg, "@​everyone") {
		t.Fatalf("expected zero-width-space neutralised mention, got %q", msg)
	}
}

func TestFormatAlert_EmojiPerCode(t *testing.T) {
	cases := map[string]string{
		CodeAwaitingUser: "⏳",
		CodeErrorLoop:    "🔁",
		CodeAuthFailed:   "🔑",
		CodeStalledTurn:  "🧊",
		CodeDiedMidTurn:  "💥",
		CodeUsageLimit:   "🪫",
	}
	for code, emoji := range cases {
		sig := baseSignal(code)
		msg := FormatAlert("myhost", sig)
		if !strings.HasPrefix(msg, emoji) {
			t.Fatalf("code %s: expected message to start with %s, got %q", code, emoji, msg)
		}
	}
}

func TestFormatAlert_CapsLength(t *testing.T) {
	sig := baseSignal(CodeErrorLoop)
	sig.Evidence = strings.Repeat("x", 5000)
	msg := FormatAlert("myhost", sig)
	if len(msg) > discordMessageCap {
		t.Fatalf("expected message capped at %d bytes, got %d", discordMessageCap, len(msg))
	}
}

func TestFormatAlert_ShortThreadUsesFirst8Chars(t *testing.T) {
	sig := baseSignal(CodeErrorLoop)
	sig.Thread = "0123456789abcdef"
	msg := FormatAlert("myhost", sig)
	if !strings.Contains(msg, "· 01234567 ") {
		t.Fatalf("expected 8-char short thread id, got %q", msg)
	}
}

func TestFormatResolved_Neutralises(t *testing.T) {
	sig := baseSignal(CodeErrorLoop)
	msg := formatResolved("myhost", sig)
	if !strings.HasPrefix(msg, "✅ resolved:") {
		t.Fatalf("expected resolved prefix, got %q", msg)
	}
}

// A question in the agent's message still counts once a pane tail follows it,
// but the pane's own chrome can't make a finished message look like a question.
func TestLooksLikeWaitingSplitsMessageAndPane(t *testing.T) {
	pane := paneEvidenceSeparator + "────────\n> \n? for shortcuts"
	if !looksLikeWaiting("Should I migrate the admin page too?" + pane) {
		t.Error("question in the message was missed behind the pane tail")
	}
	if looksLikeWaiting("Done. All tests pass." + pane) {
		t.Error("pane chrome made a finished message look like a question")
	}
	if !looksLikeWaiting("Running the migration." + paneEvidenceSeparator + "Do you want to proceed?\n❯ 1. Yes\n  2. No") {
		t.Error("menu in the pane was missed")
	}
}

// --- FIX 3: readable alert evidence (message + cleaned pane tail) ---

func TestFormatEvidence_CleansPaneChromeKeepsRealContent(t *testing.T) {
	// The shape from the real incident: a rule, a queued-message prompt
	// line with real text, another rule, and a status/keybinding line.
	pane := "────────────────\n❯ push main when merged\n────────────────\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents · 2 feedback drafts"
	evidence := "…to apply" + paneEvidenceSeparator + pane

	lines := formatEvidence(evidence)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "Pane:") {
		t.Fatalf("expected a Pane: section, got %q", joined)
	}
	if strings.Contains(joined, "────") {
		t.Errorf("expected rule-only lines to be dropped, got %q", joined)
	}
	if strings.Contains(joined, "shift+tab") || strings.Contains(joined, "for agents") {
		t.Errorf("expected the status/keybinding chrome line to be dropped, got %q", joined)
	}
	if !strings.Contains(joined, "push main when merged") {
		t.Errorf("expected the real queued-message line to be kept, got %q", joined)
	}
}

func TestFormatEvidence_OmitsPaneWhenNothingSurvivesCleaning(t *testing.T) {
	evidence := "Should I continue?" + paneEvidenceSeparator + "────\n❯ \n? for shortcuts\nesc to interrupt"
	lines := formatEvidence(evidence)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "Pane:") {
		t.Errorf("expected no Pane: section once every pane line is chrome, got %q", joined)
	}
	if !strings.Contains(joined, "Should I continue?") {
		t.Errorf("expected the message part to still be quoted, got %q", joined)
	}
}

func TestFormatEvidence_KeepsOnlyLastPaneTailKeepLines(t *testing.T) {
	var pane strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&pane, "real pane line %d\n", i)
	}
	evidence := "msg" + paneEvidenceSeparator + pane.String()
	lines := formatEvidence(evidence)
	joined := strings.Join(lines, "\n")

	for i := 0; i < 10-paneTailKeepLines; i++ {
		if strings.Contains(joined, fmt.Sprintf("real pane line %d\n", i)) {
			t.Errorf("expected early pane line %d to be dropped, got %q", i, joined)
		}
	}
	if !strings.Contains(joined, "real pane line 9") {
		t.Errorf("expected the last pane line to be kept, got %q", joined)
	}
}

func TestFormatEvidence_NoPaneSeparator_MessageOnly(t *testing.T) {
	lines := formatEvidence("just a message, no pane tail")
	if len(lines) != 1 {
		t.Fatalf("expected a single message block, got %v", lines)
	}
	if lines[0] != "> just a message, no pane tail" {
		t.Errorf("got %q", lines[0])
	}
}

func TestFormatEvidence_MessageKeepsTrueTail(t *testing.T) {
	long := strings.Repeat("word ", 100) + "Final sentence here. Should I proceed?"
	lines := formatEvidence(long)
	if len(lines) != 1 {
		t.Fatalf("expected a single message block (no pane separator present), got %v", lines)
	}
	if !strings.HasSuffix(lines[0], "Should I proceed?") {
		t.Errorf("expected the quoted block to end with the true tail, got %q", lines[0])
	}
	if len(lines[0]) > messageEvidenceCap+50 {
		t.Errorf("message block unexpectedly long: %d bytes: %q", len(lines[0]), lines[0])
	}
}

func TestTailAtBoundary_PrefersLineBoundary(t *testing.T) {
	s := strings.Repeat("x", 100) + "\n" + strings.Repeat("y", 250)
	got := tailAtBoundary(s, 350)
	want := strings.Repeat("y", 250)
	if got != want {
		t.Errorf("tailAtBoundary did not cut at the line boundary; got len=%d, want len=%d", len(got), len(want))
	}
}

func TestTailAtBoundary_PrefersSentenceBoundary(t *testing.T) {
	s := strings.Repeat("x", 100) + ". " + strings.Repeat("y", 250)
	got := tailAtBoundary(s, 350)
	want := strings.Repeat("y", 250)
	if got != want {
		t.Errorf("tailAtBoundary did not cut at the sentence boundary; got len=%d, want len=%d", len(got), len(want))
	}
}

func TestTailAtBoundary_ShortStringUnchanged(t *testing.T) {
	if got := tailAtBoundary("short", 350); got != "short" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestFormatAlert_UsesFormattedEvidence(t *testing.T) {
	sig := baseSignal(CodeAwaitingUser)
	sig.Evidence = "…to apply" + paneEvidenceSeparator + "────\n❯ push main when merged\n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents"
	msg := FormatAlert("myhost", sig)

	if !strings.Contains(msg, "Pane:") {
		t.Errorf("expected the full alert to include the cleaned pane section, got %q", msg)
	}
	if strings.Contains(msg, "shift+tab") {
		t.Errorf("expected chrome to be stripped from the full alert, got %q", msg)
	}
	if !strings.Contains(msg, "push main when merged") {
		t.Errorf("expected real pane content in the full alert, got %q", msg)
	}
}
