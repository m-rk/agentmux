package threadwatch

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sender delivers one already-formatted message (e.g. to Discord).
// Production wiring binds this to discordnotify.Send with the configured
// webhook URL.
type Sender func(message string) error

// Decision records what Handle (or Flush) did with a signal, for logging
// and for comparing shadow-mode Jev verdicts against what was actually
// sent.
type Decision struct {
	Page    bool   // a message was actually sent
	Reason  string // one line: why it did or didn't page
	Message string // the formatted message, sent or not (for logging)
}

// Alerter turns Signals into at most one Discord message each, applying
// the Jev gate (docs/design/thread-watch.md "Where Jev fits"), dedup,
// rate limiting and resolved notices (docs/design/thread-watch.md
// "Alerting"). It is safe for concurrent use.
type Alerter struct {
	cfg  Config
	host string
	send Sender
	now  func() time.Time

	mu        sync.Mutex
	lastPaged map[string]time.Time // dedup key -> last successful page
	pageTimes []time.Time          // rolling window of successful pages, host-wide
	held      int                  // pages suppressed by the rate limit, not yet reported
}

// NewAlerter builds an Alerter. now defaults to time.Now when nil.
func NewAlerter(cfg Config, host string, send Sender, now func() time.Time) *Alerter {
	if now == nil {
		now = time.Now
	}
	return &Alerter{
		cfg:       cfg,
		host:      host,
		send:      send,
		now:       now,
		lastPaged: make(map[string]time.Time),
	}
}

// Handle decides whether sig should page, and sends the message if so.
func (a *Alerter) Handle(sig Signal) Decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	if sig.Resolved {
		return a.handleResolvedLocked(sig)
	}
	if sig.Tier != TierIntervene {
		return Decision{Page: false, Reason: "insight"}
	}

	jo := evaluateJev(sig, a.cfg.Jev)
	var shadowNote string

	if sig.Code == CodeAwaitingUser {
		if jo.applied {
			if !jo.pass {
				return Decision{Page: false, Reason: "jev: " + jo.detail}
			}
			// Live mode allowed it; fall through to dedup/rate-limit.
		} else {
			// The live Jev gate did not apply here — not live mode, no
			// Judgment was scored, or it errored. The old behaviour paged
			// on every turn end followed by a long idle gap, which pages
			// every finished task whenever Jev isn't live/available. Fall
			// back to a deterministic heuristic instead: only page when
			// the tail of the evidence actually reads like a question or a
			// prompt (docs/design/thread-watch.md, "Alerting").
			mode := a.cfg.Jev.Mode
			if mode == "" {
				mode = "shadow"
			}
			rulePass := looksLikeWaiting(sig.Evidence)
			if jo.haveVerdict {
				switch {
				case !jo.pass:
					// Jev's shadow verdict also would have suppressed —
					// the same "would suppress" note every other code
					// gets in shadow/off mode.
					shadowNote = fmt.Sprintf("%s: would suppress (%s)", mode, jo.detail)
				case !rulePass:
					// Jev's shadow verdict says the session is waiting,
					// but the rule is about to suppress: log the
					// disagreement so a week of logs shows which is
					// better.
					shadowNote = fmt.Sprintf("%s: jev says waiting (%s)", mode, jo.detail)
				}
			}
			if !rulePass {
				reason := "rule: no question or prompt in the last message"
				if shadowNote != "" {
					reason = reason + "; " + shadowNote
				}
				return Decision{Page: false, Reason: reason}
			}
			// The rule says this looks like a real prompt: fall through to
			// dedup/rate-limit even though the live gate didn't apply.
		}
	} else if jo.applied {
		if !jo.pass {
			return Decision{Page: false, Reason: "jev: " + jo.detail}
		}
		// Live mode allowed it; fall through to dedup/rate-limit.
	} else if jo.haveVerdict && !jo.pass {
		// shadow/off mode (or a bypassed check) — record what live mode
		// would have done, but don't act on it (fail open).
		mode := a.cfg.Jev.Mode
		if mode == "" {
			mode = "shadow"
		}
		shadowNote = fmt.Sprintf("%s: would suppress (%s)", mode, jo.detail)
	}

	key := dedupKey(sig)
	if last, ok := a.lastPaged[key]; ok {
		if elapsed := a.now().Sub(last); elapsed < a.cfg.Alerts.Cooldown {
			reason := fmt.Sprintf("dedup: paged %s ago, cooldown %s", elapsed.Round(time.Second), a.cfg.Alerts.Cooldown)
			return Decision{Page: false, Reason: withShadowNote(shadowNote, reason)}
		}
	}

	a.pruneOldLocked()
	if a.cfg.Alerts.MaxPerHour > 0 && len(a.pageTimes) >= a.cfg.Alerts.MaxPerHour {
		a.held++
		reason := fmt.Sprintf("rate_limited: %d/%d pages in the past hour, held (%d total held)",
			len(a.pageTimes), a.cfg.Alerts.MaxPerHour, a.held)
		return Decision{Page: false, Reason: withShadowNote(shadowNote, reason), Message: FormatAlert(a.host, sig)}
	}

	msg := FormatAlert(a.host, sig)
	held := a.held
	if held > 0 {
		msg = appendHeldNote(msg, held)
	}
	if err := a.send(msg); err != nil {
		reason := fmt.Sprintf("send error: %v", err)
		return Decision{Page: false, Reason: withShadowNote(shadowNote, reason), Message: msg}
	}

	now := a.now()
	a.lastPaged[key] = now
	a.pageTimes = append(a.pageTimes, now)
	a.held = 0

	reason := "paged"
	if shadowNote != "" {
		reason = shadowNote + "; paged (jev bypassed)"
	}
	return Decision{Page: true, Reason: reason, Message: msg}
}

// Flush sends a rollup of any alerts still held by the rate limit. It is a
// no-op if nothing is held. Call it periodically (or on shutdown) so held
// alerts are eventually surfaced even if no further signal arrives to
// carry the "N more held" note.
func (a *Alerter) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held <= 0 {
		return nil
	}
	msg := fmt.Sprintf("📋 %d alert%s held in the last hour (see `agentmux threadwatch status`)", a.held, plural(a.held))
	msg = neutraliseMentions(msg)
	if err := a.send(msg); err != nil {
		return err
	}
	a.held = 0
	return nil
}

func (a *Alerter) handleResolvedLocked(sig Signal) Decision {
	key := dedupKey(sig)
	last, ok := a.lastPaged[key]
	if !ok || a.now().Sub(last) > a.cfg.Alerts.ResolvedAfter {
		return Decision{Page: false, Reason: "resolved: no recent page"}
	}
	msg := formatResolved(a.host, sig)
	if err := a.send(msg); err != nil {
		return Decision{Page: false, Reason: fmt.Sprintf("resolved send error: %v", err), Message: msg}
	}
	// The condition cleared; a fresh page should be allowed to fire again
	// without dedup treating this key as "recently paged".
	delete(a.lastPaged, key)
	return Decision{Page: true, Reason: "resolved", Message: msg}
}

func (a *Alerter) pruneOldLocked() {
	cutoff := a.now().Add(-time.Hour)
	i := 0
	for i < len(a.pageTimes) && a.pageTimes[i].Before(cutoff) {
		i++
	}
	a.pageTimes = a.pageTimes[i:]
}

func dedupKey(sig Signal) string {
	return sig.Instance + "\x00" + sig.Thread + "\x00" + sig.Code
}

func withShadowNote(shadowNote, reason string) string {
	if shadowNote == "" {
		return reason
	}
	return shadowNote + "; " + reason
}

// jevOutcome is the result of checking a signal's Judgment against the
// alert gate, independent of whether that gate is actually being enforced
// (live mode) or only observed (shadow/off, or no usable judgment).
type jevOutcome struct {
	applied     bool   // true when mode=="live" and this outcome controls paging
	haveVerdict bool   // true when a valid (non-nil, no Err) Judgment was scored
	pass        bool   // whether the signal clears the gate
	detail      string // human-readable numbers behind pass
}

// evaluateJev scores sig against jc's thresholds per
// docs/design/thread-watch.md "Alerting"/"Where Jev fits". It never
// itself decides to suppress an alert less than deterministic behaviour
// would — that's left to the caller, which only enforces `applied`
// outcomes.
func evaluateJev(sig Signal, jc JevConfig) jevOutcome {
	j := sig.Judgment
	if j == nil || j.Err != "" {
		return jevOutcome{}
	}
	pass, detail := gatePass(sig.Code, j, jc)
	return jevOutcome{
		applied:     jc.Mode == "live",
		haveVerdict: true,
		pass:        pass,
		detail:      detail,
	}
}

// gatePass implements the per-code gating rules from
// docs/design/thread-watch.md "Alerting":
//   - awaiting_user needs NeedsHumanNow >= AwaitingMinProb and a
//     WaitingKind that isn't "finished" or "none". Urgency doesn't apply:
//     a session waiting on the operator is the case worth paging even
//     when nothing is on fire.
//   - auth_failed and died_mid_turn always pass (they always page).
//   - every other intervene code needs Urgency >= PageUrgency with
//     UrgencyConf >= PageConfidence.
func gatePass(code string, j *Judgment, jc JevConfig) (bool, string) {
	switch code {
	case CodeAuthFailed, CodeDiedMidTurn:
		return true, "always pages"
	case CodeAwaitingUser:
		humanOK := j.NeedsHumanNow >= jc.AwaitingMinProb && j.WaitingKind != "finished" && j.WaitingKind != "none"
		detail := fmt.Sprintf("needs_human_now %.2f (min %.2f), waiting_kind=%s",
			j.NeedsHumanNow, jc.AwaitingMinProb, j.WaitingKind)
		return humanOK, detail
	default:
		urgencyOK := j.Urgency >= jc.PageUrgency && j.UrgencyConf >= jc.PageConfidence
		detail := fmt.Sprintf("urgency %.1f (min %.1f), confidence %.2f (min %.2f)",
			j.Urgency, jc.PageUrgency, j.UrgencyConf, jc.PageConfidence)
		return urgencyOK, detail
	}
}

// discordMessageCap leaves headroom below Discord's 2,000-char limit, same
// as dailycheck.FormatNotification.
const discordMessageCap = 1900

// alertEvidenceCap bounds the quoted evidence block inside one alert
// message; it is smaller than MaxExcerptBytes (the event/signal cap)
// because the whole message must also fit under discordMessageCap.
const alertEvidenceCap = 600

var codeEmoji = map[string]string{
	CodeAwaitingUser: "⏳",
	CodeErrorLoop:    "🔁",
	CodeAuthFailed:   "🔑",
	CodeStalledTurn:  "🧊",
	CodeDiedMidTurn:  "💥",
}

// FormatAlert renders the Discord message for one intervene signal:
// instance/thread/host, the reason, the Jev waiting_kind if known, a
// capped quoted evidence excerpt, and how to attach. Mentions are
// neutralised (as dailycheck.FormatNotification already does) and the
// result is capped at discordMessageCap.
func FormatAlert(host string, sig Signal) string {
	emoji := codeEmoji[sig.Code]
	if emoji == "" {
		emoji = "•"
	}
	head := emoji + " " + sig.Instance
	if sig.Thread != "" {
		head += " · " + shortThread(sig.Thread)
	}
	if host != "" {
		head += " on " + host
	}
	lines := []string{head}
	if sig.Reason != "" {
		lines = append(lines, sig.Reason)
	}
	if sig.Judgment != nil && sig.Judgment.WaitingKind != "" {
		lines = append(lines, "Waiting: "+sig.Judgment.WaitingKind)
	}
	if evidence := strings.TrimSpace(sig.Evidence); evidence != "" {
		lines = append(lines, quoteEvidence(evidence))
	}
	lines = append(lines, "Attach: `agentmux` → select "+sig.Instance+" → a")

	message := neutraliseMentions(strings.Join(lines, "\n"))
	return truncateMessage(message, discordMessageCap)
}

// formatResolved renders the short "condition cleared" notice for a
// dedup key that paged recently.
func formatResolved(host string, sig Signal) string {
	head := "✅ resolved: " + sig.Instance
	if sig.Thread != "" {
		head += " · " + shortThread(sig.Thread)
	}
	if host != "" {
		head += " on " + host
	}
	head += " (" + sig.Code + ")"
	return truncateMessage(neutraliseMentions(head), discordMessageCap)
}

func appendHeldNote(msg string, held int) string {
	note := fmt.Sprintf("…and %d more alert%s held (see `agentmux threadwatch status`)", held, plural(held))
	return msg + "\n\n" + note
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func shortThread(thread string) string {
	if len(thread) <= 8 {
		return thread
	}
	return thread[:8]
}

func quoteEvidence(evidence string) string {
	capped := tailCap(evidence, alertEvidenceCap)
	lines := strings.Split(capped, "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// waitingHeuristicTailBytes bounds how much of sig.Evidence looksLikeWaiting
// inspects — only the tail matters for "what is the agent waiting on right
// now".
const waitingHeuristicTailBytes = 600

// waitingPromptMarkers are case-insensitive substrings that, anywhere in the
// evidence tail, strongly suggest the agent is sitting on a question or a
// permission/approval prompt rather than reporting a finished result.
var waitingPromptMarkers = []string{
	"do you want",
	"would you like",
	"shall i",
	"should i",
	"which option",
	"approve",
	"permission",
	"(y/n)",
	"[y/n]",
	"press enter",
	"waiting for your",
	"let me know",
}

// waitingMenuCursor matches a numbered menu/option list with a selection
// cursor, e.g. "❯ 1. Yes", "> 1.", or "❯ Yes".
var waitingMenuCursor = regexp.MustCompile(`(?im)^[ \t]*[❯>][ \t]*(\d+\.|yes\b)`)

// looksLikeWaiting is the deterministic CodeAwaitingUser fallback used
// whenever the live Jev gate did not apply (docs/design/thread-watch.md,
// "Alerting"): rather than paging on every turn end followed by a long idle
// gap, it looks at the tail of the evidence for a trailing question or a
// recognizable prompt/menu.
func looksLikeWaiting(evidence string) bool {
	tail := strings.TrimSpace(tailCap(evidence, waitingHeuristicTailBytes))
	if tail == "" {
		return false
	}
	lower := strings.ToLower(tail)
	for _, marker := range waitingPromptMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if waitingMenuCursor.MatchString(tail) {
		return true
	}
	return lastLineEndsWithQuestion(tail)
}

// lastLineEndsWithQuestion reports whether the last non-empty line of tail
// ends with a question mark, once trailing quotes, markdown emphasis
// markers, and closing parens/brackets are stripped.
func lastLineEndsWithQuestion(tail string) bool {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		line = strings.TrimRightFunc(line, isTrailingDecoration)
		return strings.HasSuffix(line, "?")
	}
	return false
}

// isTrailingDecoration reports whether r is a character that can trail a
// question mark without changing whether the sentence reads as a question:
// whitespace, quotes, markdown emphasis markers, and closing brackets.
func isTrailingDecoration(r rune) bool {
	switch r {
	case ' ', '\t', '"', '\'', '`', '*', '_', ')', ']', '”', '’', '»':
		return true
	}
	return false
}

// tailCap keeps the last limit bytes of s (tails matter most for "what is
// the agent waiting on"), cutting on a UTF-8 boundary. Mirrors Excerpt in
// config.go but with alert.go's own (smaller) cap.
func tailCap(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := len(s) - limit
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return "…" + s[cut:]
}

// truncateMessage caps s at limit bytes on a UTF-8 boundary, matching
// dailycheck.FormatNotification's Discord-limit headroom.
func truncateMessage(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// neutraliseMentions defangs @mentions, including @everyone/@here, by
// inserting a zero-width space after every "@" — the same trick
// dailycheck.FormatNotification uses.
func neutraliseMentions(s string) string {
	return strings.ReplaceAll(s, "@", "@​")
}
