// Package dailycheck implements agentmux doctor: a deterministic host-wide
// health pass with a bounded analyzer escalation for unhealthy sessions. The
// analyzer can propose a small set of repairs, but this package validates and
// applies them itself; the analyzer never receives shell or tmux access.
package dailycheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

const (
	ActionNone       = "none"
	ActionStart      = "start"
	ActionRestart    = "restart"
	ActionSendEscape = "send_escape"
)

// ampUpdateDisabled reports whether name's registry opts the instance out
// of the agentmux-driven nightly update (AGENTMUX_AMP_UPDATE=off): a
// self-updating runner has no update unit by design, so the refresh-*
// platform checks must not fire for it. Only the exact value "off" opts
// out; a missing or unreadable registry means updates are on.
func ampUpdateDisabled(name string) bool {
	data, err := os.ReadFile(filepath.Join(discovery.EnvDir, name+".env"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "AGENTMUX_AMP_UPDATE=off" {
			return true
		}
	}
	return false
}

// Snapshot is the deliberately small amount of session state sent to the
// analyzer. Workdirs and registry values are omitted; pane text is capped by
// the caller before it reaches this type.
type Snapshot struct {
	Name            string        `json:"name"`
	Agent           string        `json:"agent"`
	Provider        string        `json:"provider,omitempty"`
	Model           string        `json:"model,omitempty"`
	Status          string        `json:"status"`
	LastActivity    int64         `json:"last_activity_unix,omitempty"`
	Pane            string        `json:"pane,omitempty"`
	CaptureError    string        `json:"capture_error,omitempty"`
	Issues          []HealthIssue `json:"issues,omitempty"`
	IdlePlaceholder bool          `json:"-"`
}

type Finding struct {
	Instance string `json:"instance"`
	Notable  bool   `json:"notable"`
	Finding  string `json:"finding"`
	Evidence string `json:"evidence,omitempty"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
}

type Plan struct {
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

type Analyzer interface {
	Analyze(context.Context, []Snapshot) (Plan, error)
}

type Client interface {
	ListInstances(context.Context) ([]*pb.Instance, error)
	ViewPane(context.Context, *pb.ViewPaneRequest) (*pb.ViewPaneResponse, error)
	Control(context.Context, string, pb.ControlAction) (*pb.ControlResponse, error)
	SendKeys(context.Context, *pb.SendKeysRequest) (*pb.SendKeysResponse, error)
}

type Options struct {
	PaneLines      int32
	MaxPaneBytes   int
	DryRun         bool
	PlatformProber PlatformProber
	VerifyDelay    time.Duration
	RepairAttempts map[string]RepairAttempt
}

type Outcome struct {
	Instance  string
	Finding   string
	Reason    string
	Action    string
	Result    string
	Notable   bool
	Attempted bool
}

type Report struct {
	Summary          string
	AnalysisSummary  string
	Checked          int
	BeforeIssues     []HealthIssue
	AfterIssues      []HealthIssue
	Outcomes         []Outcome
	Problems         []string
	IdlePlaceholders []string
}

// FormatNotification renders the compact notable-only report sent to
// Discord. Mentions are neutralized because analyzer text ultimately derives
// from untrusted pane contents.
func FormatNotification(host string, report Report, runErr error) string {
	var lines []string
	title := "🩺 agentmux doctor"
	if host != "" {
		title += " on " + cleanOneLine(host)
	}
	lines = append(lines, title)
	if summary := cleanOneLine(report.Summary); summary != "" {
		lines = append(lines, summary)
	}
	before := issuesByInstance(report.BeforeIssues)
	after := issuesByInstance(report.AfterIssues)
	outcomes := outcomesByInstance(report.Outcomes)
	lines = append(lines, formatGroups(groupReports(sortedIssueInstances(before, after, outcomes), before, after, outcomes))...)
	for _, problem := range report.Problems {
		lines = append(lines, "• ⚠️ "+cleanOneLine(problem))
	}
	if runErr != nil && len(report.Problems) == 0 {
		lines = append(lines, "• ⚠️ "+cleanOneLine(runErr.Error()))
	}
	message := strings.ReplaceAll(strings.Join(lines, "\n"), "@", "@\u200b")
	const discordLimit = 1900 // leave headroom below Discord's 2,000-char limit
	if len(message) > discordLimit {
		cut := discordLimit
		if i := strings.LastIndex(message[:cut], "\n"); i > 0 {
			cut = i
		}
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "\n…"
	}
	return message
}

const (
	statusBroken = iota
	statusFixed
	statusNoted
)

type groupKey struct {
	status  int
	code    string
	summary string
}

type reportGroup struct {
	key     groupKey
	members map[string]string // instance -> repair result ("" when none)
}

// groupReports buckets instances by distinct (status, issue). An issue is
// fixed when present before the recovery attempt and gone after (matched by
// code); an issue present after is still broken. Instances with only an
// observed finding and no health issues land in a "noted" group.
func groupReports(instances []string, before, after map[string][]HealthIssue, outcomes map[string]Outcome) []*reportGroup {
	groups := map[groupKey]*reportGroup{}
	var order []*reportGroup
	add := func(key groupKey, instance, result string) {
		g := groups[key]
		if g == nil {
			g = &reportGroup{key: key, members: map[string]string{}}
			groups[key] = g
			order = append(order, g)
		}
		g.members[instance] = result
	}
	for _, instance := range instances {
		outcome := outcomes[instance]
		result := ""
		if outcome.Action != "" && outcome.Action != ActionNone && outcome.Result != "" {
			result = cleanOneLine(outcome.Result)
		}
		if len(before[instance]) == 0 && len(after[instance]) == 0 {
			note := cleanOneLine(outcome.Finding)
			if note == "" {
				note = "notable issue observed"
			}
			add(groupKey{statusNoted, "", note}, instance, result)
			continue
		}
		still := map[string]bool{}
		for _, issue := range after[instance] {
			still[issue.Code] = true
			add(groupKey{statusBroken, issue.Code, cleanOneLine(issue.Summary)}, instance, result)
		}
		for _, issue := range before[instance] {
			if !still[issue.Code] {
				add(groupKey{statusFixed, issue.Code, cleanOneLine(issue.Summary)}, instance, result)
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.key.status != b.key.status {
			return a.key.status < b.key.status
		}
		if len(a.members) != len(b.members) {
			return len(a.members) > len(b.members)
		}
		if a.key.summary != b.key.summary {
			return a.key.summary < b.key.summary
		}
		return a.key.code < b.key.code
	})
	return order
}

const notificationWrapWidth = 100

// formatGroups prints each distinct issue once with its remedy, followed by
// the affected sessions (alphabetical, wrapped, never truncated).
func formatGroups(groups []*reportGroup) []string {
	var lines []string
	for _, g := range groups {
		var label string
		switch g.key.status {
		case statusBroken:
			label = "✗ Broken"
		case statusFixed:
			label = "✓ Fixed"
		default:
			label = "• Noted"
		}
		head := fmt.Sprintf("%s (%d): %s", label, len(g.members), g.key.summary)
		if g.key.status == statusBroken {
			if hint := remedyHint("NAME", g.key.code); hint != "" {
				head += " — " + hint
			}
		}
		lines = append(lines, head)
		names := make([]string, 0, len(g.members))
		for name := range g.members {
			names = append(names, name)
		}
		sort.Strings(names)
		entries := make([]string, len(names))
		for i, name := range names {
			entries[i] = name
			if r := g.members[name]; r != "" {
				entries[i] += " (" + r + ")"
			}
		}
		lines = append(lines, wrapList(entries, "   ", notificationWrapWidth)...)
	}
	return lines
}

// wrapList joins entries with ", " and wraps lines at width, indenting each.
func wrapList(entries []string, indent string, width int) []string {
	var lines []string
	cur := ""
	for i, e := range entries {
		piece := e
		if i < len(entries)-1 {
			piece += ","
		}
		if cur != "" && len(indent)+len(cur)+1+len(piece) > width {
			lines = append(lines, indent+cur)
			cur = ""
		}
		if cur != "" {
			cur += " "
		}
		cur += piece
	}
	if cur != "" {
		lines = append(lines, indent+cur)
	}
	return lines
}

// remedyHint is a short operator-facing next step for an issue code, or ""
// when no generic one exists. instance is substituted into the command
// (callers pass the placeholder NAME for grouped output).
func remedyHint(instance, code string) string {
	restart := "restart with `agentmux control -instance " + instance + " -action restart`"
	switch code {
	case "session-dead", "service-inactive", "service-failed", "process-missing", "process-mismatch",
		"backend-not-ready", "remote-disconnected", "pane-empty", "session-unknown":
		return restart
	case "stuck-menu":
		return "dismiss with `agentmux send-keys -instance " + instance + " Escape`"
	case "refresh-failed":
		return "see `journalctl -u agentmux-" + instance + "-update.service`, then `sudo systemctl start agentmux-" + instance + "-update.service`"
	case "auth-required":
		return "run `amp login` as the run user, then " + restart
	}
	return ""
}

func (r Report) Notable() bool {
	if len(r.BeforeIssues) > 0 || len(r.AfterIssues) > 0 || len(r.Problems) > 0 {
		return true
	}
	for _, outcome := range r.Outcomes {
		if outcome.Notable || outcome.Action != ActionNone {
			return true
		}
	}
	return false
}

// Run probes every session deterministically. Only unhealthy snapshots are
// sent to analyzer; its typed plan is validated before any supported
// low-blast-radius repair, and all sessions are probed again afterward. One bad
// finding does not prevent safe repairs for other instances.
func Run(ctx context.Context, client Client, analyzer Analyzer, opts Options) (Report, error) {
	if opts.MaxPaneBytes <= 0 {
		opts.MaxPaneBytes = 12 * 1024
	}

	beforeSnapshots, beforeIssues, err := collectHealth(ctx, client, opts)
	if err != nil {
		return Report{}, err
	}
	report := Report{Checked: len(beforeSnapshots), BeforeIssues: beforeIssues}
	for _, snapshot := range beforeSnapshots {
		if snapshot.IdlePlaceholder {
			report.IdlePlaceholders = append(report.IdlePlaceholders, snapshot.Name)
		}
	}
	if len(beforeSnapshots) == 0 {
		report.Summary = "No configured sessions to check."
		return report, nil
	}
	if len(beforeIssues) == 0 {
		report.Summary = fmt.Sprintf("Checked %d session(s); all healthy.", report.Checked)
		return report, nil
	}

	unhealthySnapshots := make([]Snapshot, 0, len(beforeSnapshots))
	byName := make(map[string]Snapshot, len(beforeSnapshots))
	for _, snapshot := range beforeSnapshots {
		byName[snapshot.Name] = snapshot
		if len(snapshot.Issues) > 0 {
			unhealthySnapshots = append(unhealthySnapshots, snapshot)
		}
	}
	plan, err := analyzer.Analyze(ctx, unhealthySnapshots)
	if err != nil {
		report.Problems = append(report.Problems, "doctor escalation failed: "+err.Error())
		report.AfterIssues = append([]HealthIssue(nil), report.BeforeIssues...)
		report.Summary = summarizeHealth(report)
		return report, fmt.Errorf("analyzing sessions: %w", err)
	}
	report.AnalysisSummary = strings.TrimSpace(plan.Summary)

	seen := map[string]bool{}
	repairApplied := false
	for _, finding := range plan.Findings {
		finding.Instance = strings.TrimSpace(finding.Instance)
		finding.Action = strings.TrimSpace(finding.Action)
		snapshot, ok := byName[finding.Instance]
		if !ok {
			report.Problems = append(report.Problems, fmt.Sprintf("doctor escalation returned unknown instance %q", finding.Instance))
			continue
		}
		if seen[finding.Instance] {
			report.Problems = append(report.Problems, fmt.Sprintf("doctor escalation returned more than one finding for %s", finding.Instance))
			continue
		}
		seen[finding.Instance] = true

		outcome := Outcome{
			Instance: finding.Instance,
			Finding:  cleanOneLine(finding.Finding),
			Reason:   cleanOneLine(finding.Reason),
			Action:   finding.Action,
			Notable:  finding.Notable,
		}
		if finding.Action == "" {
			finding.Action = ActionNone
			outcome.Action = ActionNone
		}

		if validationErr := validateRepair(snapshot, finding); validationErr != nil {
			outcome.Result = "not applied: " + validationErr.Error()
			outcome.Notable = true
			report.Problems = append(report.Problems, fmt.Sprintf("%s: %s", finding.Instance, outcome.Result))
			report.Outcomes = append(report.Outcomes, outcome)
			continue
		}
		if finding.Action == ActionNone {
			outcome.Result = "observed"
			report.Outcomes = append(report.Outcomes, outcome)
			continue
		}
		if opts.DryRun {
			outcome.Result = "dry run; repair not applied"
			outcome.Notable = true
			report.Outcomes = append(report.Outcomes, outcome)
			continue
		}

		fresh, freshErr := reprobeSnapshot(ctx, client, opts, finding.Instance)
		if freshErr != nil {
			outcome.Result = "not applied: could not refresh session state: " + freshErr.Error()
			outcome.Notable = true
			report.Problems = append(report.Problems, fmt.Sprintf("%s: %s", finding.Instance, outcome.Result))
			report.Outcomes = append(report.Outcomes, outcome)
			continue
		}
		if validationErr := validateRepair(fresh, finding); validationErr != nil {
			outcome.Result = "skipped: session changed since diagnosis: " + validationErr.Error()
			outcome.Notable = true
			report.Outcomes = append(report.Outcomes, outcome)
			continue
		}
		if attempt, suppressed := repairSuppressed(fresh, finding, opts.RepairAttempts); suppressed {
			outcome.Result = fmt.Sprintf("suppressed after %d unchanged ineffective %s attempts", attempt.ConsecutiveIneffective, finding.Action)
			outcome.Notable = true
			report.Problems = append(report.Problems, fmt.Sprintf("%s: %s; waiting for the session state to change", finding.Instance, outcome.Result))
			report.Outcomes = append(report.Outcomes, outcome)
			continue
		}

		outcome.Attempted = true
		result, applyErr := applyRepair(ctx, client, finding)
		if applyErr != nil {
			outcome.Result = "failed: " + applyErr.Error()
			outcome.Notable = true
			report.Problems = append(report.Problems, fmt.Sprintf("%s: repair failed: %v", finding.Instance, applyErr))
		} else {
			outcome.Result = result
			outcome.Notable = true
			repairApplied = true
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}

	if repairApplied && opts.VerifyDelay > 0 {
		select {
		case <-time.After(opts.VerifyDelay):
		case <-ctx.Done():
			report.Problems = append(report.Problems, "post-repair verification cancelled: "+ctx.Err().Error())
		}
	}
	afterSnapshots, afterIssues, afterErr := collectHealth(ctx, client, opts)
	if afterErr != nil {
		report.Problems = append(report.Problems, "post-repair verification failed: "+afterErr.Error())
		report.AfterIssues = append([]HealthIssue(nil), report.BeforeIssues...)
	} else {
		_ = afterSnapshots
		report.AfterIssues = afterIssues
	}
	report.Summary = summarizeHealth(report)
	return report, nil
}

func validateRepair(snapshot Snapshot, finding Finding) error {
	if finding.Action != "" && finding.Action != ActionNone && snapshotHasIssue(snapshot, "refresh-running") {
		return fmt.Errorf("repair is not allowed while the daily refresh is still running")
	}
	switch finding.Action {
	case ActionNone:
		return nil
	case ActionStart:
		if snapshot.Status != "dead" {
			return fmt.Errorf("start is only allowed for a dead session (status is %s)", snapshot.Status)
		}
		if !snapshotHasIssue(snapshot, "session-dead") {
			return fmt.Errorf("start requires a deterministic session-dead finding")
		}
		return nil
	case ActionRestart:
		if snapshot.Status != "idle" {
			return fmt.Errorf("restart is only allowed for an idle session (status is %s)", snapshot.Status)
		}
		if strings.TrimSpace(finding.Evidence) == "" || !strings.Contains(snapshot.Pane, finding.Evidence) {
			return fmt.Errorf("restart requires a verbatim pane excerpt as evidence")
		}
		if !snapshotHasAnyIssue(snapshot, "backend-not-ready", "pane-empty", "process-mismatch", "process-missing", "remote-disconnected", "service-failed", "service-inactive") {
			return fmt.Errorf("restart is not allowed for the deterministic findings on this session")
		}
		return nil
	case ActionSendEscape:
		if snapshot.Status != "idle" && snapshot.Status != "running" {
			return fmt.Errorf("Escape requires a live session (status is %s)", snapshot.Status)
		}
		if !looksLikeDismissableMenu(snapshot.Pane) {
			return fmt.Errorf("Escape requires a visible menu with an Escape hint")
		}
		if !snapshotHasIssue(snapshot, "stuck-menu") {
			return fmt.Errorf("Escape requires a deterministic stuck-menu finding")
		}
		return nil
	default:
		return fmt.Errorf("unsupported action %q", finding.Action)
	}
}

func reprobeSnapshot(ctx context.Context, client Client, opts Options, instance string) (Snapshot, error) {
	snapshots, _, err := collectHealth(ctx, client, opts)
	if err != nil {
		return Snapshot{}, err
	}
	for _, snapshot := range snapshots {
		if snapshot.Name == instance {
			return snapshot, nil
		}
	}
	return Snapshot{}, fmt.Errorf("instance is no longer configured")
}

func snapshotHasIssue(snapshot Snapshot, code string) bool {
	for _, issue := range snapshot.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func snapshotHasAnyIssue(snapshot Snapshot, codes ...string) bool {
	for _, code := range codes {
		if snapshotHasIssue(snapshot, code) {
			return true
		}
	}
	return false
}

func summarizeHealth(report Report) string {
	if len(report.BeforeIssues) == 0 {
		return fmt.Sprintf("Checked %d session(s); all healthy.", report.Checked)
	}
	if len(report.AfterIssues) == 0 && len(report.Problems) == 0 {
		return fmt.Sprintf("Found %d issue(s) across %d session(s); all recovered.", len(report.BeforeIssues), report.Checked)
	}
	if len(report.AfterIssues) == 0 {
		return fmt.Sprintf("Found %d issue(s) across %d session(s); session health recovered, but the doctor encountered %d problem(s).", len(report.BeforeIssues), report.Checked, len(report.Problems))
	}
	return fmt.Sprintf("Found %d issue(s) across %d session(s); %d remain.", len(report.BeforeIssues), report.Checked, len(report.AfterIssues))
}

func issuesByInstance(issues []HealthIssue) map[string][]HealthIssue {
	out := map[string][]HealthIssue{}
	for _, issue := range issues {
		out[issue.Instance] = append(out[issue.Instance], issue)
	}
	return out
}

func outcomesByInstance(outcomes []Outcome) map[string]Outcome {
	out := map[string]Outcome{}
	for _, outcome := range outcomes {
		out[outcome.Instance] = outcome
	}
	return out
}

func sortedIssueInstances(before, after map[string][]HealthIssue, outcomes map[string]Outcome) []string {
	seen := map[string]bool{}
	for instance := range before {
		seen[instance] = true
	}
	for instance := range after {
		seen[instance] = true
	}
	for instance := range outcomes {
		seen[instance] = true
	}
	instances := make([]string, 0, len(seen))
	for instance := range seen {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	return instances
}

func issueSummaries(issues []HealthIssue) string {
	if len(issues) == 0 {
		return "notable issue"
	}
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, cleanOneLine(issue.Summary))
	}
	return strings.Join(parts, "; ")
}

func applyRepair(ctx context.Context, client Client, finding Finding) (string, error) {
	switch finding.Action {
	case ActionStart:
		resp, err := client.Control(ctx, finding.Instance, pb.ControlAction_CONTROL_START)
		if err != nil {
			return "", err
		}
		if !resp.Ok {
			return "", fmt.Errorf("start: %s", resp.Message)
		}
		return "started the session", nil
	case ActionRestart:
		resp, err := client.Control(ctx, finding.Instance, pb.ControlAction_CONTROL_RESTART)
		if err != nil {
			return "", err
		}
		if !resp.Ok {
			return "", fmt.Errorf("restart: %s", resp.Message)
		}
		return "restarted the idle session", nil
	case ActionSendEscape:
		resp, err := client.SendKeys(ctx, &pb.SendKeysRequest{Instance: finding.Instance, Keys: []string{"Escape"}})
		if err != nil {
			return "", err
		}
		if !resp.Ok {
			return "", fmt.Errorf("sending Escape: %s", resp.Message)
		}
		return "dismissed the stuck menu", nil
	default:
		return "", fmt.Errorf("unsupported action %q", finding.Action)
	}
}

func looksLikeDismissableMenu(pane string) bool {
	lower := strings.ToLower(pane)
	return strings.Contains(lower, "esc to continue") ||
		strings.Contains(lower, "esc to cancel") ||
		strings.Contains(lower, "escape to continue") ||
		strings.Contains(lower, "escape to cancel")
}

func tailBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return "[earlier pane output omitted]\n" + value[len(value)-limit:]
}

func cleanOneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func statusName(status pb.Status) string {
	switch status {
	case pb.Status_STATUS_RUNNING:
		return "running"
	case pb.Status_STATUS_IDLE:
		return "idle"
	case pb.Status_STATUS_DEAD:
		return "dead"
	default:
		return "unknown"
	}
}
