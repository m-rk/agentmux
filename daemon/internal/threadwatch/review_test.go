package threadwatch

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func reviewDay(hourOffset int) time.Time {
	return time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(hourOffset) * time.Hour)
}

func TestBuildReviewStats(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	events := []Event{
		{Time: reviewDay(1), Instance: "a", Kind: KindTurnEnd, Duration: 2 * time.Minute, Tokens: Usage{InputTokens: 100, OutputTokens: 50}},
		{Time: reviewDay(2), Instance: "a", Kind: KindTurnEnd, Duration: 10 * time.Minute, Tokens: Usage{InputTokens: 200, OutputTokens: 100}},
		{Time: reviewDay(3), Instance: "a", Kind: KindAPIError},
		{Time: reviewDay(3), Instance: "a", Kind: KindToolError},
		{Time: reviewDay(3), Instance: "a", Kind: KindAuthError},
		{Time: reviewDay(3), Instance: "a", Kind: KindCompaction},
		// outside the window: must not be counted.
		{Time: reviewDay(-1), Instance: "a", Kind: KindTurnEnd, Duration: time.Hour},
		{Time: reviewDay(25), Instance: "a", Kind: KindTurnEnd, Duration: time.Hour},
	}

	got := BuildReview(events, nil, DefaultConfig(), since, until)
	s, ok := got.Stats["a"]
	if !ok {
		t.Fatalf("Stats missing instance a: %+v", got.Stats)
	}
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2", s.Turns)
	}
	if s.APIErrors != 1 || s.ToolErrors != 1 || s.AuthErrors != 1 || s.Compactions != 1 {
		t.Errorf("error/compaction counts = %+v", s)
	}
	if s.TotalTokens != 450 {
		t.Errorf("TotalTokens = %d, want 450", s.TotalTokens)
	}
	if s.P50TurnDuration != 2*time.Minute && s.P50TurnDuration != 10*time.Minute {
		t.Errorf("P50TurnDuration = %s, want one of the two samples", s.P50TurnDuration)
	}
	if s.P90TurnDuration != 10*time.Minute {
		t.Errorf("P90TurnDuration = %s, want 10m", s.P90TurnDuration)
	}
}

func TestBuildReviewExcludesReviewDisabledInstance(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	events := []Event{
		{Time: reviewDay(1), Instance: "quiet", Kind: KindTurnEnd, Duration: time.Minute},
		{Time: reviewDay(1), Instance: "loud", Kind: KindTurnEnd, Duration: time.Minute},
	}
	no := false
	cfg := DefaultConfig()
	cfg.Instances = map[string]InstanceConf{"quiet": {Review: &no}}

	got := BuildReview(events, nil, cfg, since, until)
	if _, ok := got.Stats["quiet"]; ok {
		t.Errorf("Stats has excluded instance %q: %+v", "quiet", got.Stats)
	}
	if _, ok := got.Stats["loud"]; !ok {
		t.Errorf("Stats missing included instance %q: %+v", "loud", got.Stats)
	}
}

func TestBuildReviewAlertsPagedVsSuppressed(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	cfg := DefaultConfig()
	cfg.Jev.Mode = "live"

	pagedSignal := Signal{
		Time: reviewDay(2), Instance: "a", Code: CodeErrorLoop, Tier: TierIntervene,
		Judgment: &Judgment{Urgency: 5, UrgencyConf: 0.9},
	}
	suppressedSignal := Signal{
		Time: reviewDay(3), Instance: "a", Code: CodeErrorLoop, Tier: TierIntervene,
		Judgment: &Judgment{Urgency: 1, UrgencyConf: 0.9},
	}
	// No Judgment at all: not distinguishable as suppressed, must count as
	// paged (never alert less than deterministic behaviour would).
	unjudgedSignal := Signal{Time: reviewDay(4), Instance: "a", Code: CodeAuthFailed, Tier: TierIntervene}

	got := BuildReview(nil, []Signal{pagedSignal, suppressedSignal, unjudgedSignal}, cfg, since, until)
	s := got.Stats["a"]
	if s.AlertsPaged != 2 {
		t.Errorf("AlertsPaged = %d, want 2", s.AlertsPaged)
	}
	if s.AlertsSuppressed != 1 {
		t.Errorf("AlertsSuppressed = %d, want 1", s.AlertsSuppressed)
	}
}

func TestBuildReviewClustersGroupByCategoryOrCode(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	signals := []Signal{
		{Time: reviewDay(1), Instance: "a", Code: CodeSlowTurn, Tier: TierInsight, Evidence: "slow one"},
		{Time: reviewDay(2), Instance: "a", Code: CodeSlowTurn, Tier: TierInsight, Evidence: "slow two"},
		{Time: reviewDay(3), Instance: "a", Code: CodeExpensiveTurn, Tier: TierInsight, Evidence: "expensive",
			Judgment: &Judgment{Category: "context_bloat"}},
		{Time: reviewDay(4), Instance: "b", Code: CodeCompactionChurn, Tier: TierInsight, Evidence: "churn"},
	}

	got := BuildReview(nil, signals, DefaultConfig(), since, until)
	if len(got.Clusters) != 3 {
		t.Fatalf("Clusters = %d, want 3: %+v", len(got.Clusters), got.Clusters)
	}

	var slowTurnCluster *Cluster
	for i := range got.Clusters {
		if got.Clusters[i].Tag == CodeSlowTurn && got.Clusters[i].Instance == "a" {
			slowTurnCluster = &got.Clusters[i]
		}
		if got.Clusters[i].Tag == CodeExpensiveTurn {
			t.Errorf("expensive_turn signal with a Judgment.Category should be tagged %q, not the bare code", "context_bloat")
		}
	}
	if slowTurnCluster == nil {
		t.Fatalf("no slow_turn/a cluster in %+v", got.Clusters)
	}
	if slowTurnCluster.Count != 2 {
		t.Errorf("slow_turn cluster Count = %d, want 2", slowTurnCluster.Count)
	}
	if len(slowTurnCluster.Evidence) != 2 {
		t.Errorf("slow_turn cluster Evidence = %+v, want 2 excerpts", slowTurnCluster.Evidence)
	}
}

func TestBuildReviewClustersIncludeJevSuppressedIntervene(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	cfg := DefaultConfig()
	cfg.Jev.Mode = "live"

	suppressed := Signal{
		Time: reviewDay(2), Instance: "a", Code: CodeErrorLoop, Tier: TierIntervene, Evidence: "held back",
		Judgment: &Judgment{Category: "flaky_test", Urgency: 1, UrgencyConf: 0.9},
	}
	paged := Signal{
		Time: reviewDay(3), Instance: "a", Code: CodeErrorLoop, Tier: TierIntervene,
		Judgment: &Judgment{Urgency: 5, UrgencyConf: 0.9},
	}

	got := BuildReview(nil, []Signal{suppressed, paged}, cfg, since, until)
	if len(got.Clusters) != 1 {
		t.Fatalf("Clusters = %d, want 1 (only the suppressed signal should cluster): %+v", len(got.Clusters), got.Clusters)
	}
	if got.Clusters[0].Tag != "flaky_test" || got.Clusters[0].Count != 1 {
		t.Errorf("Clusters[0] = %+v, want tag flaky_test count 1", got.Clusters[0])
	}
}

func TestBuildReviewClustersCapEvidenceBytesAndCount(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	var signals []Signal
	for i := 0; i < 5; i++ {
		signals = append(signals, Signal{
			Time: reviewDay(i), Instance: "a", Code: CodeRetryThrash, Tier: TierInsight,
			Evidence: strings.Repeat("x", 1000),
		})
	}
	got := BuildReview(nil, signals, DefaultConfig(), since, until)
	if len(got.Clusters) != 1 {
		t.Fatalf("Clusters = %d, want 1", len(got.Clusters))
	}
	c := got.Clusters[0]
	if c.Count != 5 {
		t.Errorf("Count = %d, want 5", c.Count)
	}
	if len(c.Evidence) != maxClusterEvidence {
		t.Fatalf("Evidence has %d entries, want %d", len(c.Evidence), maxClusterEvidence)
	}
	for _, ev := range c.Evidence {
		// capExcerpt (jev.go) may prepend a "…" marker (up to 3 UTF-8 bytes)
		// on top of its byte cap when it truncates, matching Excerpt's
		// existing convention in config.go.
		if len(ev) > maxClusterEvidenceBytes+len("…") {
			t.Errorf("evidence excerpt is %d bytes, want <= %d", len(ev), maxClusterEvidenceBytes+len("…"))
		}
	}
}

func TestBuildReviewTopClustersCappedAtEight(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	var signals []Signal
	for i := 0; i < 12; i++ {
		signals = append(signals, Signal{
			Time: reviewDay(i), Instance: instanceName(i), Code: CodeSlowTurn, Tier: TierInsight, Evidence: "e",
		})
	}
	got := BuildReview(nil, signals, DefaultConfig(), since, until)
	if len(got.Clusters) != maxReviewClusters {
		t.Fatalf("Clusters = %d, want %d", len(got.Clusters), maxReviewClusters)
	}
}

func instanceName(i int) string {
	return "inst" + string(rune('a'+i))
}

func TestNotable(t *testing.T) {
	if Notable(ReviewResult{}) {
		t.Error("Notable(empty) = true, want false")
	}
	if !Notable(ReviewResult{Insights: []Insight{{Title: "x"}}}) {
		t.Error("Notable(with insight) = false, want true")
	}
}

func TestFormatDigest(t *testing.T) {
	input := ReviewInput{
		Stats: map[string]InstanceReviewStats{
			"a": {Turns: 10, AlertsPaged: 2, P90TurnDuration: 12 * time.Minute},
			"b": {Turns: 5, AlertsPaged: 1, P90TurnDuration: 3 * time.Minute},
		},
	}
	result := ReviewResult{
		Summary: "fine",
		Insights: []Insight{
			{Title: "repeated permission prompt", Suggestion: "add an allow rule", Instances: []string{"a"}, Kind: "permission"},
		},
	}
	digest := FormatDigest("host1", input, result)
	if !strings.HasPrefix(digest, "🧭 agentmux daily review on host1") {
		t.Errorf("digest missing header: %q", digest)
	}
	if !strings.Contains(digest, "15 turn(s)") {
		t.Errorf("digest missing turn total: %q", digest)
	}
	if !strings.Contains(digest, "3 alert(s) sent") {
		t.Errorf("digest missing alert total: %q", digest)
	}
	if !strings.Contains(digest, "slowest: a") {
		t.Errorf("digest missing slowest instance: %q", digest)
	}
	if !strings.Contains(digest, "• repeated permission prompt — add an allow rule (a)") {
		t.Errorf("digest missing insight bullet: %q", digest)
	}
	if len(digest) > 1900 {
		t.Errorf("digest is %d bytes, want <= 1900", len(digest))
	}
}

func TestFormatDigestNeutralisesMentionsAndCapsLength(t *testing.T) {
	input := ReviewInput{Stats: map[string]InstanceReviewStats{}}
	result := ReviewResult{Insights: []Insight{
		{Title: "@everyone should look at this " + strings.Repeat("x", 3000), Suggestion: "s", Instances: []string{"a"}},
	}}
	digest := FormatDigest("", input, result)
	if strings.Contains(digest, "@everyone") {
		t.Errorf("digest leaked an unneutralised mention: %q", digest[:60])
	}
	// truncateMessage (alert.go) may append a "…" marker (up to 3 UTF-8
	// bytes) on top of its byte cap when it truncates.
	if len(digest) > reviewDigestCap+len("…") {
		t.Errorf("digest is %d bytes, want <= %d", len(digest), reviewDigestCap+len("…"))
	}
}

func TestFormatDigestNoInsights(t *testing.T) {
	digest := FormatDigest("host1", ReviewInput{Stats: map[string]InstanceReviewStats{}}, ReviewResult{})
	if !strings.Contains(digest, "Nothing cleared the bar") {
		t.Errorf("digest = %q, want a no-insights line", digest)
	}
}

func TestFormatAllQuiet(t *testing.T) {
	input := ReviewInput{Stats: map[string]InstanceReviewStats{"a": {Turns: 4}}}
	msg := FormatAllQuiet("host1", input)
	if !strings.Contains(msg, "weekly all-quiet") || !strings.Contains(msg, "4 turn(s)") {
		t.Errorf("FormatAllQuiet = %q", msg)
	}
}

func TestFormatReportContainsStatsAndClusters(t *testing.T) {
	since, until := reviewDay(0), reviewDay(24)
	input := BuildReview(
		[]Event{{Time: reviewDay(1), Instance: "a", Kind: KindTurnEnd, Duration: time.Minute}},
		[]Signal{{Time: reviewDay(2), Instance: "a", Code: CodeSlowTurn, Tier: TierInsight, Evidence: "ev-text"}},
		DefaultConfig(), since, until,
	)
	result := ReviewResult{Summary: "sum", Insights: []Insight{{Title: "t", Suggestion: "s", Kind: "test"}}}
	report := FormatReport("host1", input, result)
	for _, want := range []string{"# agentmux daily review — host1", "## Stats", "| a |", "## Insights", "### t", "## Clusters", "ev-text"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

func TestFallbackReview(t *testing.T) {
	empty := FallbackReview(ReviewInput{})
	if Notable(empty) {
		t.Errorf("FallbackReview(empty input) should not be notable: %+v", empty)
	}

	input := ReviewInput{Clusters: []Cluster{
		{Tag: "flaky_test", Instance: "a", Count: 3, Evidence: []string{"e1"}},
	}}
	result := FallbackReview(input)
	if len(result.Insights) != 1 {
		t.Fatalf("Insights = %+v, want 1", result.Insights)
	}
	if result.Insights[0].Kind != "test" {
		t.Errorf("Kind = %q, want test", result.Insights[0].Kind)
	}
	if result.Insights[0].Evidence != "e1" {
		t.Errorf("Evidence = %q, want e1", result.Insights[0].Evidence)
	}
}

// --- review_claude.go ---

func fakeReviewCommand(t *testing.T, script string) CommandFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "claude" {
			t.Fatalf("binary = %q, want claude", name)
		}
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

func TestClaudeReviewerUsesStdinNoToolsAndSchema(t *testing.T) {
	var gotArgs []string
	reviewer := ClaudeReviewer{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotArgs = append([]string(nil), args...)
			return exec.CommandContext(ctx, "sh", "-c", `printf '%s' '{"structured_output":{"summary":"ok","insights":[]}}'`)
		},
	}
	input := ReviewInput{Clusters: []Cluster{{Tag: "x", Instance: "a", Evidence: []string{"secret pane text"}}}}
	result, err := reviewer.Review(context.Background(), input)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Summary != "ok" {
		t.Errorf("Summary = %q, want ok", result.Summary)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"--tools", "--permission-mode dontAsk", "--no-session-persistence", "--json-schema"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "secret pane text") {
		t.Fatal("evidence leaked into process arguments instead of stdin")
	}
}

func TestClaudeReviewerParsesInsights(t *testing.T) {
	script := `printf '%s' '{"structured_output":{"summary":"found one","insights":[{"title":"missing allow rule","instances":["a"],"evidence":"ev","suggestion":"allow tool X","kind":"permission"}]}}'`
	reviewer := ClaudeReviewer{Command: fakeReviewCommand(t, script)}
	result, err := reviewer.Review(context.Background(), ReviewInput{})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(result.Insights) != 1 {
		t.Fatalf("Insights = %+v, want 1", result.Insights)
	}
	ins := result.Insights[0]
	if ins.Title != "missing allow rule" || ins.Kind != "permission" || ins.Suggestion != "allow tool X" {
		t.Errorf("insight = %+v", ins)
	}
}

func TestClaudeReviewerUnknownKindFallsBackToOther(t *testing.T) {
	script := `printf '%s' '{"structured_output":{"summary":"s","insights":[{"title":"t","instances":[],"evidence":"e","suggestion":"s","kind":"not_a_real_kind"}]}}'`
	reviewer := ClaudeReviewer{Command: fakeReviewCommand(t, script)}
	result, err := reviewer.Review(context.Background(), ReviewInput{})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Insights[0].Kind != "other" {
		t.Errorf("Kind = %q, want other", result.Insights[0].Kind)
	}
}

func TestClaudeReviewerResultStringEnvelope(t *testing.T) {
	script := `printf '%s' '{"result":"{\"summary\":\"one\",\"insights\":[]}"}'`
	reviewer := ClaudeReviewer{Command: fakeReviewCommand(t, script)}
	result, err := reviewer.Review(context.Background(), ReviewInput{})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Summary != "one" {
		t.Errorf("Summary = %q, want one", result.Summary)
	}
}

func TestClaudeReviewerErrorPropagates(t *testing.T) {
	reviewer := ClaudeReviewer{Command: fakeReviewCommand(t, `echo boom 1>&2; exit 1`)}
	if _, err := reviewer.Review(context.Background(), ReviewInput{}); err == nil {
		t.Fatal("Review: want error from a failing command")
	}
}

func TestClaudeReviewerNoCommandFactory(t *testing.T) {
	if _, err := (ClaudeReviewer{}).Review(context.Background(), ReviewInput{}); err == nil {
		t.Fatal("Review: want error with no Command factory")
	}
}
