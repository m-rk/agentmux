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
