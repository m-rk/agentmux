// Deterministic detectors: pure logic over the Event stream, no I/O. See
// the "Detectors" section of docs/design/thread-watch.md for the intended
// behaviour of each signal.
package threadwatch

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxSamples bounds the rolling duration/token windows used for percentile
// thresholds (slow_turn, expensive_turn).
const maxSamples = 200

// minSamplesForPercentile is how many rolling samples a percentile needs
// before a detector trusts it over a fixed fallback.
const minSamplesForPercentile = 20

// threadIdleEvictAfter is how long a thread can sit with no events before
// Tick drops its state.
const threadIdleEvictAfter = 48 * time.Hour

// openTurnStaleAfter bounds the open-turn idle-prompt check (Tick): a turn
// that has been open with no event for longer than this is treated as
// abandoned/stale rather than "probably sitting on a prompt", so it never
// fires.
const openTurnStaleAfter = 6 * time.Hour

// usageLimitAlertCooldown bounds how often a usage_limit intervene signal
// pages for the same instance, independent of alerts.cooldown: usage/rate
// limits are typically account-wide, so repeated episodes with no progress
// in between shouldn't repage every hour. A new episode that starts after
// the thread made progress (a turn succeeded, or activity resumed) bypasses
// this and pages immediately.
const usageLimitAlertCooldown = 6 * time.Hour

// usageLimitResetPattern extracts a reset-time phrase from a usage_limit
// event's excerpt (e.g. "resets 7am (UTC)" out of "You've hit your session
// limit · resets 7am (UTC)") for the signal's Reason.
var usageLimitResetPattern = regexp.MustCompile(`(?i)resets? [^·\n]{1,40}`)

// InstanceStats summarises one instance's activity for the nightly review.
type InstanceStats struct {
	Turns           int64
	P50TurnDuration time.Duration
	P90TurnDuration time.Duration
	APIErrors       int64
	ToolErrors      int64
	AuthErrors      int64
	Compactions     int64
	TotalTokens     int64
}

// Detector holds per-instance/per-thread state and turns Events into
// Signals. It is safe for concurrent use.
type Detector struct {
	cfg Config

	mu        sync.Mutex
	instances map[string]*instanceState
	threads   map[string]map[string]*threadState // instance -> thread -> state
}

// NewDetector returns a Detector using cfg's thresholds and instance
// overrides.
func NewDetector(cfg Config) *Detector {
	return &Detector{
		cfg:       cfg,
		instances: make(map[string]*instanceState),
		threads:   make(map[string]map[string]*threadState),
	}
}

// instanceState is per-instance rolling data: current status, sample
// windows for percentile thresholds, cumulative stats, and the auth_failed
// dedup/resolution state (auth failures are instance-wide, not per-thread).
type instanceState struct {
	status string

	durations []time.Duration // rolling window, oldest first, capped at maxSamples
	tokens    []int64         // rolling window, oldest first, capped at maxSamples

	turns       int64
	apiErrors   int64
	toolErrors  int64
	authErrors  int64
	compactions int64
	totalTokens int64

	authFailedActive    bool
	lastAuthFailedAlert time.Time

	// usageLimitActive/lastUsageLimitAlert/usageLimitProgressed gate the
	// usage_limit signal (docs/design/thread-watch.md, "usage_limit"):
	// usageLimitActive is true for the duration of one open episode (so
	// repeated usage-limit events while blocked don't re-signal);
	// lastUsageLimitAlert plus usageLimitAlertCooldown caps how often a new
	// episode can page; usageLimitProgressed, set when a thread recovers
	// (a turn succeeds or activity resumes after the block), lets a fresh
	// episode page immediately instead of waiting out the cooldown.
	usageLimitActive     bool
	lastUsageLimitAlert  time.Time
	usageLimitProgressed bool
}

func (is *instanceState) addDuration(d time.Duration) {
	is.durations = append(is.durations, d)
	if len(is.durations) > maxSamples {
		is.durations = is.durations[len(is.durations)-maxSamples:]
	}
}

func (is *instanceState) addTokens(n int64) {
	is.tokens = append(is.tokens, n)
	if len(is.tokens) > maxSamples {
		is.tokens = is.tokens[len(is.tokens)-maxSamples:]
	}
}

// threadState is per-thread turn/error tracking.
type threadState struct {
	open bool // activity/user_message since the last turn_end

	lastEventTime time.Time
	lastActive    time.Time // for 48h eviction

	lastAssistantExcerpt string

	// lastEventWasAPIError is true when the most recently observed event
	// (before the current one) was a KindAPIError, with nothing since to
	// recover it — used by observeTurnEndLocked to tell a turn that ended
	// normally (or with a fresh assistant message) from one an API error cut
	// short, so the latter doesn't anchor an awaiting_user window on stale
	// or absent evidence. Updated once per Observe call, after dispatch.
	lastEventWasAPIError bool

	awaitingSince      time.Time // zero when not in an awaiting-user window
	awaitingSignaled   bool
	awaitingSignalTime time.Time

	// openTurnAwaitingSignaled tracks the open-turn idle-prompt variant of
	// awaiting_user (Tick): a permission prompt or menu can block an agent
	// inside a turn that never closes, so the ordinary awaitingSince-based
	// path (anchored at turn_end/assistant_msg) never sees it. This fires
	// only while the turn is still open (awaitingSince is zero) so the two
	// paths never double-fire for the same thread.
	openTurnAwaitingSignaled bool

	consecAPIErrors      int
	apiErrorLoopSignaled bool

	toolErrorCounts       map[string]int
	toolErrorLoopSignaled map[string]bool

	turnHadError bool // any tool/api error since the turn opened

	stalledSignaled bool

	compactionTimes      []time.Time // last 24h, pruned on each compaction
	compactionSignaledOn string      // calendar day (UTC) already signaled for compaction_churn

	retryTimes    map[string][]time.Time // "tool\x00excerpt" -> timestamps in the last hour
	retrySignaled map[string]bool

	// usageLimitBlocked marks this thread as blocked by a usage_limit event
	// (docs/design/thread-watch.md, "usage_limit"): while true, awaiting_user
	// (both the post-turn and open-turn paths) and stalled_turn are skipped
	// for this thread in Tick, since a usage/rate limit — not the operator —
	// is what the session is waiting on. usageLimitSkipNext guards against
	// the turn_end/activity event that immediately follows the usage_limit
	// event as the tail of the very same interrupted turn (the real-world
	// case: an API-error assistant record immediately followed by a
	// system/turn_duration record) being mistaken for recovery: it consumes
	// exactly one such event without clearing the block, and a later one
	// does.
	usageLimitBlocked  bool
	usageLimitSkipNext bool
}

func (d *Detector) instanceStateLocked(instance string) *instanceState {
	is, ok := d.instances[instance]
	if !ok {
		is = &instanceState{}
		d.instances[instance] = is
	}
	return is
}

func (d *Detector) threadStateLocked(instance, thread string) *threadState {
	byThread, ok := d.threads[instance]
	if !ok {
		byThread = make(map[string]*threadState)
		d.threads[instance] = byThread
	}
	ts, ok := byThread[thread]
	if !ok {
		ts = &threadState{
			toolErrorCounts:       make(map[string]int),
			toolErrorLoopSignaled: make(map[string]bool),
			retryTimes:            make(map[string][]time.Time),
			retrySignaled:         make(map[string]bool),
		}
		byThread[thread] = ts
	}
	return ts
}

func (d *Detector) disabled(instance string) bool {
	ic, ok := d.cfg.Instances[instance]
	return ok && ic.Disabled
}

func newSignal(t time.Time, instance, thread, code, tier, reason, evidence string, resolved bool) Signal {
	return Signal{
		Time:     t,
		Instance: instance,
		Thread:   thread,
		Code:     code,
		Tier:     tier,
		Reason:   reason,
		Evidence: Excerpt(evidence),
		Resolved: resolved,
	}
}

// Observe updates state for ev and returns any signals it triggers.
func (d *Detector) Observe(ev Event) []Signal {
	if d.disabled(ev.Instance) {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	is := d.instanceStateLocked(ev.Instance)
	ts := d.threadStateLocked(ev.Instance, ev.Thread)
	thr := d.cfg.ThresholdsFor(ev.Instance)

	var out []Signal

	// Any event means the thread is no longer silent: clear a stalled_turn
	// alert regardless of what kind of event ended the silence.
	if ts.stalledSignaled {
		ts.stalledSignaled = false
		out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeStalledTurn, TierIntervene,
			"activity resumed", "", true))
	}

	// Same for the open-turn idle-prompt variant of awaiting_user: any
	// event on the thread means the operator (or the agent on its own)
	// moved past whatever the session was sitting on.
	if ts.openTurnAwaitingSignaled {
		ts.openTurnAwaitingSignaled = false
		out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeAwaitingUser, TierIntervene,
			"activity resumed", "", true))
	}

	ts.lastEventTime = ev.Time
	ts.lastActive = ev.Time

	switch ev.Kind {
	case KindStatus:
		is.status = ev.Status
		if ev.Status == "stopped" {
			out = append(out, d.killOpenTurnsLocked(ev.Instance, ev.Time, "session stopped while a turn was open")...)
		}

	case KindSessionExit:
		out = append(out, d.killOpenTurnsLocked(ev.Instance, ev.Time, "session exited while a turn was open")...)

	case KindUserMessage:
		if ts.awaitingSignaled {
			ts.awaitingSignaled = false
			out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeAwaitingUser, TierIntervene,
				"user replied", "", true))
			if wait := ev.Time.Sub(ts.awaitingSignalTime); wait > thr.LongWaitAfter {
				out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeLongWait, TierInsight,
					fmt.Sprintf("user replied %s after the awaiting-user alert fired", roundDuration(wait)),
					ts.lastAssistantExcerpt, false))
			}
		}
		if ts.usageLimitBlocked {
			ts.usageLimitBlocked = false
			ts.usageLimitSkipNext = false
			is.usageLimitActive = false
			out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeUsageLimit, TierIntervene,
				"user message received", "", true))
		}
		ts.awaitingSince = time.Time{}
		ts.open = true
		// A new turn starts here: any evidence carried over belongs to the
		// turn that just ended. If this new turn itself ends without a
		// fresh assistant message (an API error, an interrupt), evidence
		// should read as empty rather than a stale question from before.
		ts.lastAssistantExcerpt = ""

	case KindActivity:
		// Activity means the earlier "awaiting user" hypothesis was wrong
		// (the agent kept going on its own, e.g. a scheduled/background
		// step); stop tracking it. Only a user message counts as the human
		// having replied, so this does not emit a resolved signal.
		if ts.usageLimitBlocked {
			if ts.usageLimitSkipNext {
				// The tail of the same interrupted turn (e.g. amp/opencode
				// activity immediately following the usage-limit event);
				// stay blocked.
				ts.usageLimitSkipNext = false
			} else {
				ts.usageLimitBlocked = false
				is.usageLimitActive = false
				is.usageLimitProgressed = true
				out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeUsageLimit, TierIntervene,
					"activity resumed after the usage limit", "", true))
			}
		}
		ts.awaitingSince = time.Time{}
		ts.open = true
		ts.consecAPIErrors = 0

	case KindAssistantMsg:
		if ev.Excerpt != "" {
			ts.lastAssistantExcerpt = ev.Excerpt
		}
		ts.awaitingSince = ev.Time
		ts.awaitingSignaled = false
		ts.consecAPIErrors = 0

	case KindTurnEnd:
		if ts.usageLimitBlocked {
			if ts.usageLimitSkipNext {
				// This is the turn_end that belongs to the same broken turn
				// as the usage_limit event itself (the real-world case: an
				// isApiErrorMessage record immediately followed by a
				// system/turn_duration record); it is not recovery, so stay
				// blocked.
				ts.usageLimitSkipNext = false
			} else {
				ts.usageLimitBlocked = false
				is.usageLimitActive = false
				is.usageLimitProgressed = true
				out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeUsageLimit, TierIntervene,
					"turn completed after the usage limit", "", true))
			}
		}
		out = append(out, d.observeTurnEndLocked(is, ts, ev, thr)...)

	case KindUsageLimit:
		out = append(out, d.observeUsageLimitLocked(is, ts, ev)...)

	case KindAPIError:
		is.apiErrors++
		ts.turnHadError = true
		ts.consecAPIErrors++
		if ts.consecAPIErrors >= thr.APIErrorLoop && !ts.apiErrorLoopSignaled {
			ts.apiErrorLoopSignaled = true
			out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeErrorLoop, TierIntervene,
				fmt.Sprintf("%d consecutive API errors", ts.consecAPIErrors), ev.Excerpt, false))
		}

	case KindToolError:
		is.toolErrors++
		ts.turnHadError = true
		out = append(out, d.observeToolErrorLocked(ts, ev, thr)...)

	case KindAuthError:
		is.authErrors++
		out = append(out, d.observeAuthErrorLocked(is, ev)...)

	case KindCompaction:
		is.compactions++
		out = append(out, d.observeCompactionLocked(ts, ev, thr)...)
	}

	// Tracked after dispatch so observeTurnEndLocked (called from the
	// KindTurnEnd case above) still sees whether the *previous* event was an
	// API error, not this turn_end itself.
	ts.lastEventWasAPIError = ev.Kind == KindAPIError

	return out
}

// killOpenTurnsLocked emits died_mid_turn for every thread of instance that
// currently has an open turn, and closes them. Status/session-exit events
// are instance-wide, so this scans all of the instance's threads rather
// than relying on ev.Thread (which is often empty for these kinds).
func (d *Detector) killOpenTurnsLocked(instance string, t time.Time, reason string) []Signal {
	var out []Signal
	for thread, ts := range d.threads[instance] {
		if !ts.open {
			continue
		}
		ts.open = false
		out = append(out, newSignal(t, instance, thread, CodeDiedMidTurn, TierIntervene, reason, ts.lastAssistantExcerpt, false))
	}
	return out
}

func (d *Detector) observeTurnEndLocked(is *instanceState, ts *threadState, ev Event, thr Thresholds) []Signal {
	var out []Signal

	ts.open = false
	if ev.Excerpt != "" {
		ts.lastAssistantExcerpt = ev.Excerpt
	}
	if ts.lastAssistantExcerpt != "" || !ts.lastEventWasAPIError {
		ts.awaitingSince = ev.Time
	} else {
		// The turn ended with no fresh assistant message, and the last
		// thing that happened on this thread was an API error: there is no
		// real evidence the session is "waiting on a question" (the
		// incident this guards against: an API-error record immediately
		// followed by a turn_end, with awaiting_user then reusing a stale
		// message from much earlier as its evidence). Don't anchor an
		// awaiting_user window for it at all.
		ts.awaitingSince = time.Time{}
	}
	ts.awaitingSignaled = false
	ts.consecAPIErrors = 0

	if ts.apiErrorLoopSignaled {
		ts.apiErrorLoopSignaled = false
		out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeErrorLoop, TierIntervene,
			"turn completed successfully", "", true))
	}
	for tool, signaled := range ts.toolErrorLoopSignaled {
		if signaled {
			out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeErrorLoop, TierIntervene,
				fmt.Sprintf("tool %q recovered", tool), "", true))
		}
	}
	ts.toolErrorCounts = make(map[string]int)
	ts.toolErrorLoopSignaled = make(map[string]bool)

	// stalled_turn, if it had fired, was already resolved at the top of
	// Observe (this event is itself the activity that ends the silence).

	if is.authFailedActive {
		is.authFailedActive = false
		out = append(out, newSignal(ev.Time, ev.Instance, "", CodeAuthFailed, TierIntervene,
			"turn completed successfully after an earlier auth failure", "", true))
	}

	if ts.turnHadError {
		ts.turnHadError = false
		out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeRecoveredErrors, TierInsight,
			"turn had tool/API errors but completed", ev.Excerpt, false))
	}

	is.turns++

	if ev.Duration > 0 {
		n := len(is.durations)
		var slow bool
		if n >= minSamplesForPercentile {
			p90 := percentileDuration(is.durations, 0.9)
			slow = ev.Duration > thr.SlowTurnMin && ev.Duration > p90
			if slow {
				out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeSlowTurn, TierInsight,
					fmt.Sprintf("turn took %s, over the instance's p90 of %s", roundDuration(ev.Duration), roundDuration(p90)),
					ev.Excerpt, false))
			}
		} else {
			slow = ev.Duration > thr.SlowTurnMin*2
			if slow {
				out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeSlowTurn, TierInsight,
					fmt.Sprintf("turn took %s, over twice the slow-turn floor", roundDuration(ev.Duration)),
					ev.Excerpt, false))
			}
		}
		is.addDuration(ev.Duration)
	}

	total := ev.Tokens.InputTokens + ev.Tokens.OutputTokens + ev.Tokens.CacheReadTokens + ev.Tokens.CacheWriteTokens
	if total > 0 {
		n := len(is.tokens)
		if n >= minSamplesForPercentile {
			p95 := percentileInt64(is.tokens, 0.95)
			if total > p95 {
				reason := fmt.Sprintf("turn used %d tokens, over the instance's p95 of %d", total, p95)
				if ev.Tokens.CostUSD > 0 {
					reason = fmt.Sprintf("%s (cost $%.4f)", reason, ev.Tokens.CostUSD)
				}
				out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeExpensiveTurn, TierInsight,
					reason, ev.Excerpt, false))
			}
		}
		is.addTokens(total)
		is.totalTokens += total
	}

	return out
}

func (d *Detector) observeToolErrorLocked(ts *threadState, ev Event, thr Thresholds) []Signal {
	var out []Signal

	tool := ev.Tool
	if tool == "" {
		tool = "unknown"
	}
	ts.toolErrorCounts[tool]++
	if ts.toolErrorCounts[tool] >= thr.ToolErrorLoop && !ts.toolErrorLoopSignaled[tool] {
		ts.toolErrorLoopSignaled[tool] = true
		out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeErrorLoop, TierIntervene,
			fmt.Sprintf("tool %q failed %d times in this turn", tool, ts.toolErrorCounts[tool]), ev.Excerpt, false))
	}

	// retry_thrash: identical (tool, excerpt) repeated within an hour, even
	// across turns.
	key := tool + "\x00" + ev.Excerpt
	times := append(ts.retryTimes[key], ev.Time)
	times = pruneOlderThan(times, ev.Time.Add(-time.Hour))
	ts.retryTimes[key] = times
	if len(times) >= thr.RetryThrashRepeats {
		if !ts.retrySignaled[key] {
			ts.retrySignaled[key] = true
			out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeRetryThrash, TierInsight,
				fmt.Sprintf("tool %q repeated the same error %d times in the last hour", tool, len(times)), ev.Excerpt, false))
		}
	} else {
		ts.retrySignaled[key] = false
	}

	return out
}

func (d *Detector) observeAuthErrorLocked(is *instanceState, ev Event) []Signal {
	cooldown := d.cfg.Alerts.Cooldown
	if !is.lastAuthFailedAlert.IsZero() && ev.Time.Sub(is.lastAuthFailedAlert) < cooldown {
		return nil
	}
	is.lastAuthFailedAlert = ev.Time
	is.authFailedActive = true
	return []Signal{newSignal(ev.Time, ev.Instance, ev.Thread, CodeAuthFailed, TierIntervene,
		"authentication failed", ev.Excerpt, false)}
}

// observeUsageLimitLocked handles a KindUsageLimit event: it always blocks
// the thread (awaiting_user/stalled_turn stop firing for it in Tick until it
// clears — see threadState.usageLimitBlocked), but only emits an intervene
// signal once per open episode, and — once an episode has closed — no more
// than once per usageLimitAlertCooldown per instance unless the thread made
// progress since the last one (is.usageLimitProgressed).
func (d *Detector) observeUsageLimitLocked(is *instanceState, ts *threadState, ev Event) []Signal {
	ts.usageLimitBlocked = true
	ts.usageLimitSkipNext = true
	ts.awaitingSince = time.Time{}
	ts.awaitingSignaled = false
	ts.openTurnAwaitingSignaled = false
	ts.consecAPIErrors = 0

	if is.usageLimitActive {
		// Still the same open episode (e.g. a retry hit the limit again):
		// already signaled.
		return nil
	}
	if !is.lastUsageLimitAlert.IsZero() && ev.Time.Sub(is.lastUsageLimitAlert) < usageLimitAlertCooldown && !is.usageLimitProgressed {
		is.usageLimitActive = true
		return nil
	}

	is.usageLimitActive = true
	is.lastUsageLimitAlert = ev.Time
	is.usageLimitProgressed = false
	return []Signal{newSignal(ev.Time, ev.Instance, ev.Thread, CodeUsageLimit, TierIntervene,
		usageLimitReason(ev.Excerpt), ev.Excerpt, false)}
}

// usageLimitReason builds the CodeUsageLimit signal's Reason, pulling the
// reset-time phrase out of excerpt when present (e.g. "You've hit your
// session limit · resets 7am (UTC)" -> "usage limit reached — resets 7am
// (UTC)").
func usageLimitReason(excerpt string) string {
	reason := "usage limit reached"
	if m := strings.TrimSpace(usageLimitResetPattern.FindString(excerpt)); m != "" {
		reason += " — " + m
	}
	return reason
}

func (d *Detector) observeCompactionLocked(ts *threadState, ev Event, thr Thresholds) []Signal {
	ts.compactionTimes = append(ts.compactionTimes, ev.Time)
	ts.compactionTimes = pruneOlderThan(ts.compactionTimes, ev.Time.Add(-24*time.Hour))

	var out []Signal
	day := ev.Time.UTC().Format("2006-01-02")
	if len(ts.compactionTimes) > thr.CompactionsPerDay && ts.compactionSignaledOn != day {
		ts.compactionSignaledOn = day
		out = append(out, newSignal(ev.Time, ev.Instance, ev.Thread, CodeCompactionChurn, TierInsight,
			fmt.Sprintf("%d compactions in the last 24h", len(ts.compactionTimes)), ev.Excerpt, false))
	}
	return out
}

func pruneOlderThan(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return times
	}
	return times[i:]
}

// Tick runs time-based detectors and bounds memory. Call it roughly every
// 30s with the current wall-clock time and the current instance list
// (whose Status reflects discovery, not events).
func (d *Detector) Tick(now time.Time, instances []Instance) []Signal {
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []Signal

	for _, inst := range instances {
		if d.disabled(inst.Name) {
			continue
		}
		is := d.instanceStateLocked(inst.Name)
		is.status = inst.Status
		thr := d.cfg.ThresholdsFor(inst.Name)

		for thread, ts := range d.threads[inst.Name] {
			if ts.usageLimitBlocked {
				// A usage/rate limit, not the operator, is what this thread
				// is waiting on: the dedicated usage_limit signal already
				// covers it (see observeUsageLimitLocked), so awaiting_user
				// and stalled_turn stay quiet until it clears.
				continue
			}

			// awaiting_user
			if !ts.awaitingSince.IsZero() && !ts.awaitingSignaled && is.status == "idle" {
				if wait := now.Sub(ts.awaitingSince); wait >= thr.AwaitingUserAfter {
					ts.awaitingSignaled = true
					ts.awaitingSignalTime = now
					out = append(out, newSignal(now, inst.Name, thread, CodeAwaitingUser, TierIntervene,
						fmt.Sprintf("waiting on the user for %s with no reply", roundDuration(wait)),
						ts.lastAssistantExcerpt, false))
				}
			}

			// stalled_turn
			if ts.open && is.status == "running" && !ts.lastEventTime.IsZero() {
				if silence := now.Sub(ts.lastEventTime); silence >= thr.StalledTurnAfter && !ts.stalledSignaled {
					ts.stalledSignaled = true
					out = append(out, newSignal(now, inst.Name, thread, CodeStalledTurn, TierIntervene,
						fmt.Sprintf("no activity for %s while the turn is running", roundDuration(silence)),
						ts.lastAssistantExcerpt, false))
				}
			}
		}

		// awaiting_user (open-turn idle prompt): a permission prompt or menu
		// blocks an agent inside a turn that never ends, so discovery
		// reports the pane "idle" while neither the ordinary
		// awaitingSince-based awaiting_user (needs a turn end) nor
		// stalled_turn (needs status "running") ever fires. Only the
		// instance's most-recently-active open thread is considered, so a
		// host with several long-open (but not actually stuck) threads
		// doesn't fire once per thread.
		if is.status == "idle" {
			if thread, ts := d.mostRecentOpenTurnLocked(inst.Name, now); ts != nil && !ts.openTurnAwaitingSignaled {
				if silence := now.Sub(ts.lastEventTime); silence >= thr.AwaitingUserAfter {
					ts.openTurnAwaitingSignaled = true
					out = append(out, newSignal(now, inst.Name, thread, CodeAwaitingUser, TierIntervene,
						fmt.Sprintf("turn open and the session idle for %s — likely a prompt or menu", roundDuration(silence)),
						ts.lastAssistantExcerpt, false))
				}
			}
		}
	}

	// Evict idle threads.
	for instance, byThread := range d.threads {
		for thread, ts := range byThread {
			if !ts.lastActive.IsZero() && now.Sub(ts.lastActive) > threadIdleEvictAfter {
				delete(byThread, thread)
			}
		}
		if len(byThread) == 0 {
			delete(d.threads, instance)
		}
	}

	return out
}

// mostRecentOpenTurnLocked returns the thread (and its state) with the most
// recent activity among instance's open turns that have no
// awaitingSince anchor yet (i.e. the ordinary post-turn-end/assistant_msg
// awaiting_user path does not already cover them — see threadState's
// openTurnAwaitingSignaled doc comment). Threads whose last event is older
// than openTurnStaleAfter are excluded so a long-abandoned open turn never
// counts as "most recent". Returns ("", nil) when there is no candidate.
func (d *Detector) mostRecentOpenTurnLocked(instance string, now time.Time) (string, *threadState) {
	var bestThread string
	var best *threadState
	for thread, ts := range d.threads[instance] {
		if !ts.open || !ts.awaitingSince.IsZero() || ts.usageLimitBlocked {
			continue
		}
		if ts.lastEventTime.IsZero() || now.Sub(ts.lastEventTime) > openTurnStaleAfter {
			continue
		}
		if best == nil || ts.lastEventTime.After(best.lastEventTime) {
			bestThread, best = thread, ts
		}
	}
	return bestThread, best
}

// Stats returns rolling/cumulative counts for instance, for the nightly
// review. Returns the zero value for an instance the detector has not
// observed.
func (d *Detector) Stats(instance string) InstanceStats {
	d.mu.Lock()
	defer d.mu.Unlock()

	is, ok := d.instances[instance]
	if !ok {
		return InstanceStats{}
	}
	return InstanceStats{
		Turns:           is.turns,
		P50TurnDuration: percentileDuration(is.durations, 0.5),
		P90TurnDuration: percentileDuration(is.durations, 0.9),
		APIErrors:       is.apiErrors,
		ToolErrors:      is.toolErrors,
		AuthErrors:      is.authErrors,
		Compactions:     is.compactions,
		TotalTokens:     is.totalTokens,
	}
}

func roundDuration(d time.Duration) time.Duration {
	return d.Round(time.Minute)
}

func percentileDuration(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[percentileIndex(len(sorted), p)]
}

func percentileInt64(samples []int64, p float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[percentileIndex(len(sorted), p)]
}

func percentileIndex(n int, p float64) int {
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}
