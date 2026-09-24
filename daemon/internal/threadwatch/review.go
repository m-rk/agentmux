// Nightly review aggregation and formatting: turns the last window of
// Events/Signals into per-instance stats and ranked insight clusters (pure,
// no I/O), and formats the results for Discord and the on-disk report. See
// docs/design/thread-watch.md, "Nightly review and digest". The bounded
// Claude escalation that turns clusters into prose insights lives in
// review_claude.go.
package threadwatch

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxReviewClusters bounds how many insight clusters BuildReview keeps
// (ranked by Score, descending) — the "top 8 clusters" from the design doc.
const maxReviewClusters = 8

// maxClusterEvidence bounds how many evidence excerpts a Cluster keeps.
const maxClusterEvidence = 3

// maxClusterEvidenceBytes caps each Cluster evidence excerpt. It is smaller
// than Event/Signal's own MaxExcerptBytes cap because a cluster can quote
// up to maxClusterEvidence of them and the review payload as a whole must
// stay small and cheap to send to Claude.
const maxClusterEvidenceBytes = 400

// reviewDigestCap mirrors dailycheck.FormatNotification's Discord-limit
// headroom (see alert.go's discordMessageCap).
const reviewDigestCap = 1900

// maxDigestInsights bounds how many insights FormatDigest quotes as
// bullets; the full list still goes to FormatReport.
const maxDigestInsights = 5

// InstanceReviewStats summarises one instance's activity over a review
// window: turn counts and timing, error/compaction counts, tokens, and how
// many intervene alerts were paged versus held by the Jev gate. Unlike
// Detector.Stats, this is computed purely from the Events/Signals passed to
// BuildReview for one bounded window — it has no dependency on the live
// Detector/Alerter's in-memory dedup, rate-limit, or rolling-percentile
// state, none of which round-trips through the store.
type InstanceReviewStats struct {
	Turns            int64
	P50TurnDuration  time.Duration
	P90TurnDuration  time.Duration
	APIErrors        int64
	ToolErrors       int64
	AuthErrors       int64
	Compactions      int64
	TotalTokens      int64
	AlertsPaged      int64 // intervene signals that were not suppressed by a live Jev gate
	AlertsSuppressed int64 // intervene signals a live Jev gate would have (or did) hold
}

// evidenceCandidate is Cluster's working state while BuildReview accumulates
// evidence; only the most recent maxClusterEvidence survive into the
// finished Cluster.Evidence.
type evidenceCandidate struct {
	time time.Time
	text string
}

// Cluster groups insight-tier signals — plus intervene signals a live Jev
// gate suppressed, when a Judgment makes that distinguishable — by
// (category-or-code, instance) for the nightly review's ranking (design
// doc: "Insight tagging at write time" / "Ranks insight clusters in code").
type Cluster struct {
	Tag      string // Judgment.Category when any member signal has one set, else Signal.Code
	Instance string
	Codes    []string // distinct Signal.Code values contributing, sorted
	Count    int
	Score    float64 // count x cost weight (tokens/duration when known) x recency, summed per member
	LastSeen time.Time
	Evidence []string // up to maxClusterEvidence, most recent first, each capped to maxClusterEvidenceBytes

	evidenceCandidates []evidenceCandidate
}

// ReviewInput is the aggregated, review-window-scoped state BuildReview
// produces: what the bounded Claude escalation (or the -no-model fallback)
// turns into a ReviewResult.
type ReviewInput struct {
	Since    time.Time
	Until    time.Time
	Stats    map[string]InstanceReviewStats
	Clusters []Cluster // top maxReviewClusters by Score, descending
}

// Insight is one operator-facing suggestion in a ReviewResult.
type Insight struct {
	Title      string
	Instances  []string
	Evidence   string
	Suggestion string
	Kind       string // agents_md | permission | threshold | test | workflow | other
}

// ReviewResult is the nightly review's output, whether produced by the
// Claude escalation (review_claude.go) or FallbackReview.
type ReviewResult struct {
	Summary  string
	Insights []Insight
}

// Notable reports whether result has anything worth telling the operator
// about. An empty Insights list is a normal, expected outcome (the design
// doc: "skip anything speculative; empty list is fine"), so it is the only
// signal used here — Summary alone never makes a review notable.
func Notable(result ReviewResult) bool {
	return len(result.Insights) > 0
}

// BuildReview aggregates events and signals with Time in [since, until]
// into per-instance stats and ranked insight clusters. Instances with
// cfg.Instances[x].Review == false are excluded entirely (design doc:
// "Per-instance jev: off / review: off in threadwatch.yaml keeps a
// sensitive project local-only").
func BuildReview(events []Event, signals []Signal, cfg Config, since, until time.Time) ReviewInput {
	stats := map[string]*InstanceReviewStats{}
	durations := map[string][]time.Duration{}

	statFor := func(instance string) *InstanceReviewStats {
		s, ok := stats[instance]
		if !ok {
			s = &InstanceReviewStats{}
			stats[instance] = s
		}
		return s
	}

	for _, e := range events {
		if !inWindow(e.Time, since, until) || !reviewEnabled(cfg, e.Instance) {
			continue
		}
		s := statFor(e.Instance)
		switch e.Kind {
		case KindTurnEnd:
			s.Turns++
			if e.Duration > 0 {
				durations[e.Instance] = append(durations[e.Instance], e.Duration)
			}
			s.TotalTokens += totalTokens(e.Tokens)
		case KindAPIError:
			s.APIErrors++
		case KindToolError:
			s.ToolErrors++
		case KindAuthError:
			s.AuthErrors++
		case KindCompaction:
			s.Compactions++
		}
	}
	for instance, durs := range durations {
		s := statFor(instance)
		s.P50TurnDuration = percentileDuration(durs, 0.5)
		s.P90TurnDuration = percentileDuration(durs, 0.9)
	}

	turnIndex := indexTurnEnds(events)
	clusters := map[string]*Cluster{}

	for _, sig := range signals {
		if !inWindow(sig.Time, since, until) || !reviewEnabled(cfg, sig.Instance) {
			continue
		}

		switch {
		case sig.Tier == TierInsight:
			addToCluster(clusters, sig, turnIndex, until)

		case sig.Tier == TierIntervene && !sig.Resolved:
			s := statFor(sig.Instance)
			// jev-suppressed is only distinguishable from a signal's own
			// Judgment when a live gate actually acted on it; shadow mode
			// (or no verdict at all) leaves alerting behaviour unchanged,
			// so those signals count as paged here, matching what the
			// Alerter actually did (modulo dedup/rate-limiting, whose
			// live-only state does not round-trip through the store).
			jo := evaluateJev(sig, cfg.Jev)
			if jo.applied && !jo.pass {
				s.AlertsSuppressed++
				addToCluster(clusters, sig, turnIndex, until)
			} else {
				s.AlertsPaged++
			}
		}
	}

	out := ReviewInput{
		Since: since,
		Until: until,
		Stats: make(map[string]InstanceReviewStats, len(stats)),
	}
	for instance, s := range stats {
		out.Stats[instance] = *s
	}
	out.Clusters = topClusters(clusters)
	return out
}

func inWindow(t, since, until time.Time) bool {
	return !t.Before(since) && !t.After(until)
}

// reviewEnabled reports whether instance's activity should be included in
// the nightly review. Default true; only an explicit review: false excludes
// it (nil, the common case, means "not set" and defaults on).
func reviewEnabled(cfg Config, instance string) bool {
	if ic, ok := cfg.Instances[instance]; ok && ic.Review != nil {
		return *ic.Review
	}
	return true
}

func totalTokens(u Usage) int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// indexTurnEnds keys every turn_end event by (instance, thread, time), so
// costWeight can look up the Duration/Tokens behind a slow_turn,
// expensive_turn, or recovered_errors signal — all three are emitted from
// observeTurnEndLocked at the same ev.Time as the turn_end event itself.
func indexTurnEnds(events []Event) map[string]Event {
	idx := make(map[string]Event)
	for _, e := range events {
		if e.Kind != KindTurnEnd {
			continue
		}
		idx[turnKey(e.Instance, e.Thread, e.Time)] = e
	}
	return idx
}

func turnKey(instance, thread string, t time.Time) string {
	return instance + "\x00" + thread + "\x00" + t.UTC().Format(time.RFC3339Nano)
}

func addToCluster(clusters map[string]*Cluster, sig Signal, turnIndex map[string]Event, until time.Time) {
	tag := sig.Code
	if sig.Judgment != nil && sig.Judgment.Category != "" {
		tag = sig.Judgment.Category
	}
	key := tag + "\x00" + sig.Instance

	c, ok := clusters[key]
	if !ok {
		c = &Cluster{Tag: tag, Instance: sig.Instance}
		clusters[key] = c
	}
	c.Count++
	c.Score += costWeight(sig, turnIndex) * recencyWeight(sig.Time, until)
	if sig.Time.After(c.LastSeen) {
		c.LastSeen = sig.Time
	}
	if !containsString(c.Codes, sig.Code) {
		c.Codes = append(c.Codes, sig.Code)
		sort.Strings(c.Codes)
	}
	if evidence := strings.TrimSpace(sig.Evidence); evidence != "" {
		c.evidenceCandidates = append(c.evidenceCandidates, evidenceCandidate{
			time: sig.Time,
			text: capExcerpt(evidence, maxClusterEvidenceBytes),
		})
	}
}

// costWeight approximates the resource cost behind sig: the actual turn
// duration/token usage when sig lines up with a turn_end event (slow_turn,
// expensive_turn, recovered_errors), or a flat weight of 1 for signals with
// no such natural cost (compaction_churn, retry_thrash, long_wait, and
// jev-suppressed intervene signals).
func costWeight(sig Signal, turnIndex map[string]Event) float64 {
	e, ok := turnIndex[turnKey(sig.Instance, sig.Thread, sig.Time)]
	if !ok {
		return 1.0
	}
	w := 1.0
	if e.Duration > 0 {
		w += e.Duration.Minutes() * 0.2
	}
	if total := totalTokens(e.Tokens); total > 0 {
		w += float64(total) / 10000.0
	}
	return w
}

// recencyWeight favours signals closer to the end of the review window: 1.0
// at until, decaying to 0.5 a day earlier.
func recencyWeight(t, until time.Time) float64 {
	age := until.Sub(t).Hours()
	if age < 0 {
		age = 0
	}
	return 1.0 / (1.0 + age/24.0)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// topClusters finalises each cluster's Evidence (most recent
// maxClusterEvidence excerpts) and returns the top maxReviewClusters by
// Score, with deterministic tie-breaks.
func topClusters(clusters map[string]*Cluster) []Cluster {
	list := make([]Cluster, 0, len(clusters))
	for _, c := range clusters {
		sort.Slice(c.evidenceCandidates, func(i, j int) bool {
			return c.evidenceCandidates[i].time.After(c.evidenceCandidates[j].time)
		})
		n := len(c.evidenceCandidates)
		if n > maxClusterEvidence {
			n = maxClusterEvidence
		}
		for i := 0; i < n; i++ {
			c.Evidence = append(c.Evidence, c.evidenceCandidates[i].text)
		}
		c.evidenceCandidates = nil
		list = append(list, *c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Score != list[j].Score {
			return list[i].Score > list[j].Score
		}
		if !list[i].LastSeen.Equal(list[j].LastSeen) {
			return list[i].LastSeen.After(list[j].LastSeen)
		}
		if list[i].Tag != list[j].Tag {
			return list[i].Tag < list[j].Tag
		}
		return list[i].Instance < list[j].Instance
	})
	if len(list) > maxReviewClusters {
		list = list[:maxReviewClusters]
	}
	return list
}

// FallbackReview builds a ReviewResult straight from input's clusters,
// without calling Claude. It backs both -no-model and a failed Claude
// review, so the digest and report always say something grounded in the
// deterministic stats even when the model step is skipped or unavailable.
func FallbackReview(input ReviewInput) ReviewResult {
	if len(input.Clusters) == 0 {
		return ReviewResult{Summary: "No notable clusters in this window."}
	}
	n := len(input.Clusters)
	if n > maxDigestInsights {
		n = maxDigestInsights
	}
	insights := make([]Insight, 0, n)
	for _, c := range input.Clusters[:n] {
		insights = append(insights, Insight{
			Title:      clusterTitle(c),
			Instances:  []string{c.Instance},
			Evidence:   firstEvidence(c),
			Suggestion: "no model was available to suggest a fix; review the evidence directly",
			Kind:       kindForTag(c.Tag),
		})
	}
	return ReviewResult{
		Summary:  fmt.Sprintf("%d cluster(s) from stats only (no model review)", len(input.Clusters)),
		Insights: insights,
	}
}

func clusterTitle(c Cluster) string {
	return fmt.Sprintf("%s x%d on %s", humanizeTag(c.Tag), c.Count, c.Instance)
}

func firstEvidence(c Cluster) string {
	if len(c.Evidence) == 0 {
		return ""
	}
	return c.Evidence[0]
}

func humanizeTag(tag string) string {
	return strings.ReplaceAll(tag, "_", " ")
}

// kindForTag maps a cluster's tag — a Jev category when Judgment tagging
// ran, otherwise a bare Signal code — to the closest Insight.Kind, for
// FallbackReview (the Claude review chooses its own Kind per insight).
func kindForTag(tag string) string {
	switch tag {
	case "missing_permission":
		return "permission"
	case "flaky_test", CodeRetryThrash:
		return "test"
	case "repeated_instruction", "unclear_task":
		return "agents_md"
	case "context_bloat", CodeSlowTurn, CodeExpensiveTurn, CodeCompactionChurn:
		return "threshold"
	case "env_or_tooling", "external_outage", "auth", CodeLongWait, CodeAuthFailed:
		return "workflow"
	default:
		return "other"
	}
}

// FormatDigest renders the Discord digest: a headline, then up to
// maxDigestInsights bullets. Mentions are neutralised and the result is
// capped at reviewDigestCap, matching FormatAlert's Discord-limit headroom.
func FormatDigest(host string, input ReviewInput, result ReviewResult) string {
	title := "🧭 agentmux daily review"
	if host != "" {
		title += " on " + host
	}
	lines := []string{title, headline(input)}

	n := len(result.Insights)
	if n > maxDigestInsights {
		n = maxDigestInsights
	}
	for _, ins := range result.Insights[:n] {
		line := "• " + ins.Title
		if ins.Suggestion != "" {
			line += " — " + ins.Suggestion
		}
		if len(ins.Instances) > 0 {
			line += " (" + strings.Join(ins.Instances, ", ") + ")"
		}
		lines = append(lines, line)
	}
	if n == 0 {
		lines = append(lines, "Nothing cleared the bar for a suggestion.")
	}

	message := neutraliseMentions(strings.Join(lines, "\n"))
	return truncateMessage(message, reviewDigestCap)
}

// FormatAllQuiet renders the optional weekly "nothing to report" line (see
// -weekly-quiet): the same headline stats as FormatDigest, with no insight
// bullets, sent instead of nothing on a week with no notable review.
func FormatAllQuiet(host string, input ReviewInput) string {
	title := "🧭 agentmux weekly all-quiet"
	if host != "" {
		title += " on " + host
	}
	message := neutraliseMentions(title + " — " + headline(input))
	return truncateMessage(message, reviewDigestCap)
}

// headline renders the one-line stats summary shared by FormatDigest and
// FormatAllQuiet: total turns, alerts sent (and held, if any), and the
// slowest instance by p90 turn duration.
func headline(input ReviewInput) string {
	var totalTurns, alertsPaged, alertsSuppressed int64
	var slowInstance string
	var slowP90 time.Duration
	for _, instance := range sortedInstances(input.Stats) {
		s := input.Stats[instance]
		totalTurns += s.Turns
		alertsPaged += s.AlertsPaged
		alertsSuppressed += s.AlertsSuppressed
		if s.P90TurnDuration > slowP90 {
			slowP90 = s.P90TurnDuration
			slowInstance = instance
		}
	}
	parts := []string{
		fmt.Sprintf("%d turn(s)", totalTurns),
		fmt.Sprintf("%d alert(s) sent", alertsPaged),
	}
	if alertsSuppressed > 0 {
		parts = append(parts, fmt.Sprintf("%d held by the model gate", alertsSuppressed))
	}
	if slowInstance != "" {
		parts = append(parts, fmt.Sprintf("slowest: %s (p90 %s)", slowInstance, slowP90.Round(time.Second)))
	}
	return strings.Join(parts, " · ")
}

func sortedInstances(stats map[string]InstanceReviewStats) []string {
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FormatReport renders the full Markdown report written to
// ReviewDir(home)/YYYY-MM-DD.md: a stats table for every instance, every
// insight with its evidence, and every cluster BuildReview kept (so a human
// can audit what the Claude review saw, whether or not it said anything).
func FormatReport(host string, input ReviewInput, result ReviewResult) string {
	var b strings.Builder

	title := "# agentmux daily review"
	if host != "" {
		title += " — " + host
	}
	fmt.Fprintf(&b, "%s\n\n", title)
	fmt.Fprintf(&b, "Window: %s to %s (UTC)\n\n", input.Since.UTC().Format(time.RFC3339), input.Until.UTC().Format(time.RFC3339))
	if result.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", result.Summary)
	}

	b.WriteString("## Stats\n\n")
	b.WriteString("| Instance | Turns | p50 turn | p90 turn | API err | Tool err | Auth err | Compactions | Tokens | Paged | Suppressed |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, instance := range sortedInstances(input.Stats) {
		s := input.Stats[instance]
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %d | %d | %d | %d | %d | %d | %d |\n",
			instance, s.Turns, s.P50TurnDuration.Round(time.Second), s.P90TurnDuration.Round(time.Second),
			s.APIErrors, s.ToolErrors, s.AuthErrors, s.Compactions, s.TotalTokens, s.AlertsPaged, s.AlertsSuppressed)
	}
	b.WriteString("\n")

	if len(result.Insights) > 0 {
		b.WriteString("## Insights\n\n")
		for _, ins := range result.Insights {
			fmt.Fprintf(&b, "### %s\n\n", ins.Title)
			if len(ins.Instances) > 0 {
				fmt.Fprintf(&b, "- Instances: %s\n", strings.Join(ins.Instances, ", "))
			}
			if ins.Kind != "" {
				fmt.Fprintf(&b, "- Kind: %s\n", ins.Kind)
			}
			if ins.Suggestion != "" {
				fmt.Fprintf(&b, "- Suggestion: %s\n", ins.Suggestion)
			}
			if ins.Evidence != "" {
				fmt.Fprintf(&b, "- Evidence: %s\n", ins.Evidence)
			}
			b.WriteString("\n")
		}
	}

	if len(input.Clusters) > 0 {
		b.WriteString("## Clusters\n\n")
		for _, c := range input.Clusters {
			fmt.Fprintf(&b, "### %s on %s (x%d, score %.2f)\n\n", humanizeTag(c.Tag), c.Instance, c.Count, c.Score)
			if len(c.Codes) > 0 {
				fmt.Fprintf(&b, "- Codes: %s\n", strings.Join(c.Codes, ", "))
			}
			fmt.Fprintf(&b, "- Last seen: %s\n", c.LastSeen.UTC().Format(time.RFC3339))
			for _, ev := range c.Evidence {
				fmt.Fprintf(&b, "- Evidence: %s\n", ev)
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}
