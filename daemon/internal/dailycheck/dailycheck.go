// Package dailycheck implements agentmux doctor: a deterministic host-wide
// health pass with a bounded analyzer escalation for unhealthy sessions. The
// analyzer can propose a small set of repairs, but this package validates and
// applies them itself; the analyzer never receives shell or tmux access.
package dailycheck

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

const (
	ActionNone       = "none"
	ActionStart      = "start"
	ActionRestart    = "restart"
	ActionSendEscape = "send_escape"
)

// Snapshot is the deliberately small amount of session state sent to the
// analyzer. Workdirs and registry values are omitted; pane text is capped by
// the caller before it reaches this type.
type Snapshot struct {
	Name         string        `json:"name"`
	Agent        string        `json:"agent"`
	Provider     string        `json:"provider,omitempty"`
	Model        string        `json:"model,omitempty"`
	Status       string        `json:"status"`
	LastActivity int64         `json:"last_activity_unix,omitempty"`
	Pane         string        `json:"pane,omitempty"`
	CaptureError string        `json:"capture_error,omitempty"`
	Issues       []HealthIssue `json:"issues,omitempty"`
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
}

type Outcome struct {
	Instance string
	Finding  string
	Reason   string
	Action   string
	Result   string
	Notable  bool
}

type Report struct {
	Summary         string
	AnalysisSummary string
	Checked         int
	BeforeIssues    []HealthIssue
	AfterIssues     []HealthIssue
	Outcomes        []Outcome
	Problems        []string
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
	for _, instance := range sortedIssueInstances(before, after, outcomes) {
		line := "• " + instance + ": " + issueSummaries(before[instance])
		if outcome, ok := outcomes[instance]; ok && outcome.Action != ActionNone && outcome.Action != "" {
			line += " — " + outcome.Result
		}
		if len(after[instance]) == 0 {
			line += " — healthy after check"
		} else {
			line += " — still: " + issueSummaries(after[instance])
		}
		lines = append(lines, line)
	}
	for _, problem := range report.Problems {
		lines = append(lines, "• ⚠️ "+cleanOneLine(problem))
	}
	if runErr != nil && len(report.Problems) == 0 {
		lines = append(lines, "• ⚠️ "+cleanOneLine(runErr.Error()))
	}
	message := strings.ReplaceAll(strings.Join(lines, "\n"), "@", "@\u200b")
	const discordLimit = 1900 // leave headroom below Discord's 2,000-char limit
	if len(message) > discordLimit {
		message = message[:discordLimit] + "…"
	}
	return message
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
