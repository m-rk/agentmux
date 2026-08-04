package dailycheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

type fakeAnalyzer struct {
	plan      Plan
	err       error
	snapshots []Snapshot
	calls     int
}

func (a *fakeAnalyzer) Analyze(_ context.Context, snapshots []Snapshot) (Plan, error) {
	a.calls++
	a.snapshots = snapshots
	return a.plan, a.err
}

func TestRunDoesNotCallClaudeForHealthySessions(t *testing.T) {
	client := &fakeClient{
		instances: []*pb.Instance{{Name: "healthy", Agent: "opencode", Status: pb.Status_STATUS_IDLE, TmuxSession: "healthy", Pid: 42}},
		panes:     map[string]string{"healthy": "ready for input"},
	}
	analyzer := &fakeAnalyzer{err: errors.New("must not run")}
	report, err := Run(context.Background(), client, analyzer, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if analyzer.calls != 0 {
		t.Fatalf("Claude was called %d time(s) for a healthy session", analyzer.calls)
	}
	if report.Notable() || !strings.Contains(report.Summary, "all healthy") {
		t.Fatalf("report = %+v", report)
	}
}

func TestCoreHealthIssuesAreBackendAware(t *testing.T) {
	claude := &pb.Instance{Name: "claude", Agent: "claude-code", Status: pb.Status_STATUS_IDLE, TmuxSession: "claude", Pid: 1}
	issues := coreHealthIssues(claude, Snapshot{Pane: "❯\n"})
	if len(issues) != 1 || issues[0].Code != "remote-disconnected" {
		t.Fatalf("Claude issues = %+v", issues)
	}
	kilo := &pb.Instance{Name: "kilo", Agent: "kilo", Status: pb.Status_STATUS_IDLE, TmuxSession: "kilo", Pid: 1}
	issues = coreHealthIssues(kilo, Snapshot{Pane: "ctrl+p commands\n"})
	if len(issues) != 1 || issues[0].Code != "remote-disconnected" {
		t.Fatalf("Kilo issues = %+v", issues)
	}
}

type fakeClient struct {
	instances []*pb.Instance
	panes     map[string]string
	controls  []string
	keys      []string
	onControl func(string, pb.ControlAction)
}

func (c *fakeClient) ListInstances(context.Context) ([]*pb.Instance, error) {
	return c.instances, nil
}

func (c *fakeClient) ViewPane(_ context.Context, req *pb.ViewPaneRequest) (*pb.ViewPaneResponse, error) {
	return &pb.ViewPaneResponse{Content: c.panes[req.Instance]}, nil
}

func (c *fakeClient) Control(_ context.Context, instance string, action pb.ControlAction) (*pb.ControlResponse, error) {
	c.controls = append(c.controls, instance+":"+action.String())
	if c.onControl != nil {
		c.onControl(instance, action)
	}
	return &pb.ControlResponse{Ok: true}, nil
}

func TestRunEscalatesOnlyUnhealthySessionsAndVerifiesRecovery(t *testing.T) {
	dead := &pb.Instance{Name: "dead", Agent: "opencode", Status: pb.Status_STATUS_DEAD}
	client := &fakeClient{
		instances: []*pb.Instance{
			dead,
			{Name: "healthy", Agent: "opencode", Status: pb.Status_STATUS_IDLE, TmuxSession: "healthy", Pid: 42},
		},
		panes: map[string]string{"healthy": "ready", "dead": "ready"},
	}
	client.onControl = func(instance string, action pb.ControlAction) {
		if instance == "dead" && action == pb.ControlAction_CONTROL_START {
			dead.Status = pb.Status_STATUS_IDLE
			dead.TmuxSession = "dead"
			dead.Pid = 99
		}
	}
	analyzer := &fakeAnalyzer{plan: Plan{Findings: []Finding{{Instance: "dead", Finding: "not running", Action: ActionStart}}}}
	report, err := Run(context.Background(), client, analyzer, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(analyzer.snapshots) != 1 || analyzer.snapshots[0].Name != "dead" {
		t.Fatalf("escalated snapshots = %+v", analyzer.snapshots)
	}
	if len(report.BeforeIssues) != 1 || len(report.AfterIssues) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Summary, "all recovered") {
		t.Fatalf("summary = %q", report.Summary)
	}
}

func (c *fakeClient) SendKeys(_ context.Context, req *pb.SendKeysRequest) (*pb.SendKeysResponse, error) {
	c.keys = append(c.keys, req.Instance+":"+strings.Join(req.Keys, ","))
	return &pb.SendKeysResponse{Ok: true}, nil
}

func TestRunAppliesConservativeRepairs(t *testing.T) {
	client := &fakeClient{
		instances: []*pb.Instance{
			{Name: "dead", Status: pb.Status_STATUS_DEAD},
			{Name: "menu", Agent: "claude-code", Status: pb.Status_STATUS_IDLE, TmuxSession: "menu", Pid: 2},
			{Name: "stuck", Status: pb.Status_STATUS_IDLE},
		},
		panes: map[string]string{
			"menu":  "Disconnect this session\nEnter to select · Esc to continue",
			"stuck": "fatal: connection closed\n",
		},
	}
	analyzer := &fakeAnalyzer{plan: Plan{
		Summary: "three sessions need help",
		Findings: []Finding{
			{Instance: "dead", Notable: true, Finding: "not running", Action: ActionStart},
			{Instance: "menu", Notable: true, Finding: "menu open", Action: ActionSendEscape},
			{Instance: "stuck", Notable: true, Finding: "terminal failure", Evidence: "fatal: connection closed", Action: ActionRestart},
		},
	}}

	report, err := Run(context.Background(), client, analyzer, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := strings.Join(client.controls, ","), "dead:CONTROL_START,stuck:CONTROL_RESTART"; got != want {
		t.Fatalf("controls = %q, want %q", got, want)
	}
	if got, want := strings.Join(client.keys, ","), "menu:Escape"; got != want {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	if !report.Notable() {
		t.Fatal("report should be notable")
	}
}

func TestRunRejectsUnsafeRestart(t *testing.T) {
	client := &fakeClient{
		instances: []*pb.Instance{{Name: "busy", Status: pb.Status_STATUS_RUNNING}},
		panes:     map[string]string{"busy": "fatal: this text is part of a conversation"},
	}
	analyzer := &fakeAnalyzer{plan: Plan{Findings: []Finding{{
		Instance: "busy", Finding: "restart it", Evidence: "fatal: this text is part of a conversation", Action: ActionRestart,
	}}}}

	report, err := Run(context.Background(), client, analyzer, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.controls) != 0 {
		t.Fatalf("unsafe control was applied: %v", client.controls)
	}
	if len(report.Problems) != 1 || !strings.Contains(report.Problems[0], "only allowed for an idle session") {
		t.Fatalf("problems = %v", report.Problems)
	}
}

func TestValidateRestartRequiresCompatibleDeterministicIssue(t *testing.T) {
	snapshot := Snapshot{
		Status: "idle",
		Pane:   "fatal text",
		Issues: []HealthIssue{{Code: "refresh-failed"}},
	}
	err := validateRepair(snapshot, Finding{Action: ActionRestart, Evidence: "fatal text"})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRejectsEscapeWithoutVisibleHint(t *testing.T) {
	client := &fakeClient{
		instances: []*pb.Instance{{Name: "prompt", Status: pb.Status_STATUS_IDLE}},
		panes:     map[string]string{"prompt": "> "},
	}
	analyzer := &fakeAnalyzer{plan: Plan{Findings: []Finding{{Instance: "prompt", Action: ActionSendEscape}}}}

	report, err := Run(context.Background(), client, analyzer, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.keys) != 0 || len(report.Problems) != 1 {
		t.Fatalf("keys = %v, problems = %v", client.keys, report.Problems)
	}
}

func TestRunCapsPaneAndDoesNotInspectDeadSession(t *testing.T) {
	client := &fakeClient{
		instances: []*pb.Instance{
			{Name: "alive", Status: pb.Status_STATUS_IDLE},
			{Name: "dead", Status: pb.Status_STATUS_DEAD},
		},
		panes: map[string]string{"alive": strings.Repeat("x", 2048)},
	}
	analyzer := &fakeAnalyzer{plan: Plan{Findings: []Finding{}}}

	if _, err := Run(context.Background(), client, analyzer, Options{MaxPaneBytes: 1024}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := analyzer.snapshots[0].Pane; len(got) > 1100 || !strings.HasPrefix(got, "[earlier pane output omitted]") {
		t.Fatalf("pane was not capped correctly: len=%d prefix=%q", len(got), got[:20])
	}
	if analyzer.snapshots[1].Pane != "" {
		t.Fatal("dead session pane should not be captured")
	}
}

func TestRunReturnsAnalyzerErrorAsNotableProblem(t *testing.T) {
	client := &fakeClient{instances: []*pb.Instance{{Name: "one", Status: pb.Status_STATUS_DEAD}}}
	analyzer := &fakeAnalyzer{err: errors.New("login expired")}
	report, err := Run(context.Background(), client, analyzer, Options{})
	if err == nil || !report.Notable() || !strings.Contains(report.Problems[0], "login expired") {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestFormatNotificationNeutralizesMentions(t *testing.T) {
	report := Report{Summary: "found @everyone", Outcomes: []Outcome{{
		Instance: "one", Finding: "needs attention @here", Action: ActionNone, Notable: true, Result: "observed",
	}}}
	message := FormatNotification("host", report, nil)
	if strings.Contains(message, "@everyone") || strings.Contains(message, "@here") {
		t.Fatalf("Discord mentions were not neutralized: %q", message)
	}
}
