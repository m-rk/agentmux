// Runner wires the collectors, the detector, the Jev gates, and the
// alerter into a poll loop. See docs/design/thread-watch.md, "Phases" and
// "Running it and secrets", and cmd/agentmux/threadwatch_cmd.go for the CLI
// that builds one.
package threadwatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/typesafe"
)

const (
	// defaultPollInterval is how often Run polls instances when
	// Runner.PollInterval is left zero.
	defaultPollInterval = 20 * time.Second

	// defaultInsightsPerHour bounds JudgeInsight calls when
	// Runner.InsightsPerHour is left zero, so a noisy host doesn't run up a
	// TypeSafe bill tagging every insight signal.
	defaultInsightsPerHour = 30

	// judgeTimeout bounds a single Judge/JudgeInsight call so a slow or
	// hanging TypeSafe request can never stall a poll cycle.
	judgeTimeout = 15 * time.Second

	// recentEventsCap bounds the in-memory ring of recent events kept per
	// instance for Judge/JudgeInsight context.
	recentEventsCap = 200
)

// InstanceLister lists the instances a Runner should watch. *tuiclient.Client
// (the daemon's own gRPC client) satisfies this directly.
type InstanceLister interface {
	ListInstances(ctx context.Context) ([]*pb.Instance, error)
}

// PaneViewer renders an instance's current tmux pane as text, for the
// stall-detection fallback used on agents with no structured collector
// (docs/design/thread-watch.md, "any (fallback)").
type PaneViewer interface {
	ViewPane(ctx context.Context, instance string) (string, error)
}

// InsightJudge is implemented by a Judge that can also tag insight-tier
// signals for the nightly review (docs/design/thread-watch.md, "Insight
// tagging at write time"). JevJudge implements it; Runner type-asserts for
// it rather than requiring every Judge to.
type InsightJudge interface {
	JudgeInsight(ctx context.Context, sig Signal, recent []Event) Judgment
}

// NewJudge returns a JevJudge backed by TYPESAFE_API_KEY, or nil when no key
// is configured or cfg.Jev.Mode is "off". A nil Judge means Runner runs
// deterministic-only, per the design doc's "never alert less because Jev is
// down".
func NewJudge(cfg Config) Judge {
	if cfg.Jev.Mode == "off" {
		return nil
	}
	client, ok := typesafe.NewFromEnv()
	if !ok {
		return nil
	}
	return JevJudge{Client: client, Model: cfg.Jev.Model}
}

// Runner polls instances on an interval, feeds their agents' events through
// a Detector, asks Judge (if any) about intervene/insight signals, and hands
// the result to Alerter. Every exported field can be set directly by a
// caller (cmd/agentmux/threadwatch_cmd.go) or by a test; Detector defaults
// to NewDetector(Config) when left nil.
type Runner struct {
	Config  Config
	Store   *Store
	Offsets *FileOffsets
	Lister  InstanceLister
	Alerter *Alerter

	// PaneViewer is optional: when set, instances whose agent has no
	// structured collector (see newCollectorForAgent) fall back to a
	// pane-hash heartbeat instead of being invisible to thread watch.
	PaneViewer PaneViewer

	// Judge is optional. When set and an instance allows it
	// (Config.JevAllowed), intervene signals are scored with Judge.Judge and
	// insight signals with JudgeInsight, if Judge implements InsightJudge.
	Judge Judge

	Detector *Detector

	Host            string
	PollInterval    time.Duration
	InsightsPerHour int

	// Clock lets tests control "now"; defaults to time.Now.
	Clock func() time.Time

	// OnDecision, if set, is called once per signal produced this cycle
	// after Alerter.Handle has run — the CLI's -dry-run mode uses it to
	// print what would have paged without needing a real Discord send.
	OnDecision func(Signal, Decision)

	collectors   map[string]Collector
	lastSeen     map[string]Instance
	paneHashes   map[string]string
	recent       map[string][]Event
	insightTimes []time.Time
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return defaultPollInterval
}

func (r *Runner) insightsPerHour() int {
	if r.InsightsPerHour > 0 {
		return r.InsightsPerHour
	}
	return defaultInsightsPerHour
}

func (r *Runner) disabled(instance string) bool {
	ic, ok := r.Config.Instances[instance]
	return ok && ic.Disabled
}

// Run polls forever (until ctx is canceled), running one Cycle immediately
// and then on PollInterval, flushing the alerter's held-alert rollup hourly
// and pruning the store daily. A single Cycle error is logged, not fatal:
// one bad poll (a transient daemon dial failure, say) shouldn't kill the
// process, since the whole point of thread watch is to notice trouble.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()
	flushTicker := time.NewTicker(time.Hour)
	defer flushTicker.Stop()
	pruneTicker := time.NewTicker(24 * time.Hour)
	defer pruneTicker.Stop()

	if err := r.Cycle(ctx); err != nil {
		log.Printf("threadwatch: cycle error: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.Cycle(ctx); err != nil {
				log.Printf("threadwatch: cycle error: %v", err)
			}
		case <-flushTicker.C:
			if r.Alerter != nil {
				if err := r.Alerter.Flush(); err != nil {
					log.Printf("threadwatch: flush error: %v", err)
				}
			}
		case <-pruneTicker.C:
			if r.Store != nil {
				if err := r.Store.Prune(r.now(), 0); err != nil {
					log.Printf("threadwatch: prune error: %v", err)
				}
			}
		}
	}
}

// Cycle runs one poll: list instances, poll each one's collector, synthesize
// status/session-exit events, run them through the Detector, judge and
// alert on the resulting signals, and persist events/signals/offsets. It is
// the unit `threadwatch serve -once` runs and serve_test.go exercises
// directly.
func (r *Runner) Cycle(ctx context.Context) error {
	if r.Alerter == nil {
		return errors.New("threadwatch: Runner.Alerter is required")
	}
	if r.Lister == nil {
		return errors.New("threadwatch: Runner.Lister is required")
	}
	if r.Store == nil {
		return errors.New("threadwatch: Runner.Store is required")
	}
	if r.Detector == nil {
		r.Detector = NewDetector(r.Config)
	}
	if r.collectors == nil {
		r.collectors = map[string]Collector{}
	}
	if r.lastSeen == nil {
		r.lastSeen = map[string]Instance{}
	}
	if r.paneHashes == nil {
		r.paneHashes = map[string]string{}
	}
	if r.recent == nil {
		r.recent = map[string][]Event{}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("threadwatch: resolving home directory: %w", err)
	}

	pbInstances, err := r.Lister.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("threadwatch: listing instances: %w", err)
	}

	now := r.now()
	seen := make(map[string]bool, len(pbInstances))
	var allEvents []Event
	var instances []Instance

	for _, pbi := range pbInstances {
		inst := instanceFromPB(home, pbi)
		seen[inst.Name] = true
		instances = append(instances, inst)
		if r.disabled(inst.Name) {
			continue
		}

		prev, existed := r.lastSeen[inst.Name]
		if !existed || prev.Status != inst.Status {
			allEvents = append(allEvents, Event{
				Time: now, Instance: inst.Name, Agent: inst.Agent,
				Kind: KindStatus, Status: inst.Status,
			})
		}
		r.lastSeen[inst.Name] = inst

		events, pollErr := r.pollInstance(ctx, inst)
		if pollErr != nil {
			log.Printf("threadwatch: %s: poll error: %v", inst.Name, pollErr)
		}
		allEvents = append(allEvents, events...)
	}

	// Instances that dropped out of the list since the last cycle (removed,
	// or the daemon lost track of them) get a session_exit so any open turn
	// is closed out as died_mid_turn rather than silently going stale.
	for name, prev := range r.lastSeen {
		if seen[name] {
			continue
		}
		allEvents = append(allEvents, Event{
			Time: now, Instance: name, Agent: prev.Agent, Kind: KindSessionExit,
		})
		delete(r.lastSeen, name)
		delete(r.collectors, name)
	}

	if len(allEvents) > 0 {
		if err := r.Store.AppendEvents(allEvents); err != nil {
			log.Printf("threadwatch: appending events: %v", err)
		}
	}

	var signals []Signal
	for _, ev := range allEvents {
		r.pushRecent(ev)
		signals = append(signals, r.Detector.Observe(ev)...)
	}
	signals = append(signals, r.Detector.Tick(now, instances)...)

	stored := make([]Signal, 0, len(signals))
	for _, sig := range signals {
		r.judge(ctx, &sig)

		decision := r.Alerter.Handle(sig)
		if !decision.Page && strings.HasPrefix(decision.Reason, "jev:") {
			// Live-mode Jev rejected an intervene signal: it still happened
			// and is still worth the nightly review, just not worth paging
			// on, so it's stored (and ranked) as an insight instead.
			sig.Tier = TierInsight
		}

		log.Printf("threadwatch: %s %s %s: %s", sig.Instance, sig.Code, sig.Tier, decision.Reason)
		if r.OnDecision != nil {
			r.OnDecision(sig, decision)
		}
		stored = append(stored, sig)
	}
	if len(stored) > 0 {
		if err := r.Store.AppendSignals(stored); err != nil {
			log.Printf("threadwatch: appending signals: %v", err)
		}
	}

	if r.Offsets != nil {
		if err := r.Offsets.Save(); err != nil {
			log.Printf("threadwatch: saving offsets: %v", err)
		}
	}

	log.Printf("threadwatch: cycle done: %d instances, %d events, %d signals", len(instances), len(allEvents), len(signals))
	return nil
}

// pollInstance polls inst's collector (creating and caching one per
// instance name the first time it's seen, so a collector's in-memory state
// — tool-id caches, amp turn state — survives across cycles) or, for an
// agent with no structured collector, falls back to a pane-hash heartbeat
// when a PaneViewer is configured. ErrSourceMissing is treated as "nothing
// to report", never logged as an error.
func (r *Runner) pollInstance(ctx context.Context, inst Instance) ([]Event, error) {
	c, ok := r.collectors[inst.Name]
	if !ok {
		c = newCollectorForAgent(inst.Agent)
		r.collectors[inst.Name] = c
	}
	if c == nil {
		if r.PaneViewer == nil {
			return nil, nil
		}
		return r.pollPaneFallback(ctx, inst), nil
	}

	events, err := c.Poll(ctx, inst, r.Offsets)
	if err != nil {
		if errors.Is(err, ErrSourceMissing) {
			return nil, nil
		}
		return nil, err
	}
	return events, nil
}

// pollPaneFallback emits a KindActivity event whenever an instance's pane
// content has changed since the last poll, and nothing when it hasn't. That
// gives the Detector's stalled_turn/awaiting_user logic something to work
// from for agents thread watch has no structured collector for (kilo, zero,
// custom providers, ...), per docs/design/thread-watch.md's "any (fallback)"
// source. A ViewPane error is not fatal — it just means no event this poll.
func (r *Runner) pollPaneFallback(ctx context.Context, inst Instance) []Event {
	content, err := r.PaneViewer.ViewPane(ctx, inst.Name)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	prev, existed := r.paneHashes[inst.Name]
	r.paneHashes[inst.Name] = hash
	if existed && prev == hash {
		return nil
	}
	return []Event{{Time: r.now(), Instance: inst.Name, Agent: inst.Agent, Kind: KindActivity}}
}

// newCollectorForAgent returns a fresh Collector for agent, or nil when
// thread watch has no structured collector for it (docs/design/thread-watch.md's
// source table: claude-code, amp, and opencode are covered; everything else
// relies on the pane fallback).
func newCollectorForAgent(agent string) Collector {
	switch agent {
	case "claude-code":
		return &ClaudeCollector{}
	case "amp":
		return &AmpCollector{}
	case "opencode":
		return &OpencodeCollector{}
	default:
		return nil
	}
}

// judge scores sig with Runner.Judge in place, when one is configured and
// the instance allows sending excerpts to TypeSafe. Resolved signals are
// never judged — they're just "condition cleared" notices. Intervene
// signals always get Judge.Judge; insight signals only get JudgeInsight
// when Judge implements it and the hourly insight-judging budget isn't
// exhausted, since insight signals fire far more often than intervene ones.
func (r *Runner) judge(ctx context.Context, sig *Signal) {
	if sig.Resolved || r.Judge == nil || !r.Config.JevAllowed(sig.Instance) {
		return
	}

	switch sig.Tier {
	case TierIntervene:
		jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
		defer cancel()
		recent := r.recentEvents(sig.Instance, sig.Thread, maxRecentEvents)
		jm := r.Judge.Judge(jctx, *sig, recent)
		sig.Judgment = &jm

	case TierInsight:
		insighter, ok := r.Judge.(InsightJudge)
		if !ok || !r.allowInsightJudge(r.now()) {
			return
		}
		jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
		defer cancel()
		recent := r.recentEvents(sig.Instance, sig.Thread, maxRecentEvents)
		jm := insighter.JudgeInsight(jctx, *sig, recent)
		sig.Judgment = &jm
	}
}

// allowInsightJudge reports whether another JudgeInsight call is within the
// hourly budget, recording it if so.
func (r *Runner) allowInsightJudge(now time.Time) bool {
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(r.insightTimes) && r.insightTimes[i].Before(cutoff) {
		i++
	}
	r.insightTimes = r.insightTimes[i:]
	if len(r.insightTimes) >= r.insightsPerHour() {
		return false
	}
	r.insightTimes = append(r.insightTimes, now)
	return true
}

// pushRecent appends ev to instance's bounded recent-event ring, used as
// Judge/JudgeInsight context (docs/design/thread-watch.md's per-signal Jev
// requests carry the trailing events, not the whole day's log).
func (r *Runner) pushRecent(ev Event) {
	list := append(r.recent[ev.Instance], ev)
	if len(list) > recentEventsCap {
		list = list[len(list)-recentEventsCap:]
	}
	r.recent[ev.Instance] = list
}

// recentEvents returns the last n events for instance, optionally filtered
// to one thread (thread == "" matches every thread, for pane-fallback
// events and instance-wide kinds like session_exit).
func (r *Runner) recentEvents(instance, thread string, n int) []Event {
	all := r.recent[instance]
	var out []Event
	for _, e := range all {
		if thread == "" || e.Thread == thread {
			out = append(out, e)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func instanceFromPB(home string, pbi *pb.Instance) Instance {
	return Instance{
		Name:    pbi.Name,
		Agent:   pbi.Agent,
		Workdir: pbi.Workdir,
		Home:    home,
		Status:  statusFromPB(pbi.Status),
	}
}

func statusFromPB(s pb.Status) string {
	switch s {
	case pb.Status_STATUS_RUNNING:
		return "running"
	case pb.Status_STATUS_IDLE:
		return "idle"
	case pb.Status_STATUS_DEAD:
		return "stopped"
	default:
		return "unknown"
	}
}
