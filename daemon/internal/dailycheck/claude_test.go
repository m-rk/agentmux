package dailycheck

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestClaudeAnalyzerUsesStdinAndNoTools(t *testing.T) {
	var gotArgs []string
	analyzer := ClaudeAnalyzer{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name != "claude" {
				t.Fatalf("binary = %q", name)
			}
			gotArgs = append([]string(nil), args...)
			return exec.CommandContext(ctx, "sh", "-c", `printf '%s' '{"structured_output":{"summary":"fine","findings":[]}}'`)
		},
	}
	if _, err := analyzer.Analyze(context.Background(), []Snapshot{{Name: "one", Pane: "secret pane text"}}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"--tools", "--permission-mode dontAsk", "--no-session-persistence", "--json-schema"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "secret pane text") {
		t.Fatal("pane text leaked into process arguments instead of stdin")
	}
}

func TestParseClaudePlanStructuredOutput(t *testing.T) {
	out := []byte(`{"type":"result","structured_output":{"summary":"fine","findings":[]}}`)
	plan, err := parseClaudePlan(out)
	if err != nil {
		t.Fatalf("parseClaudePlan: %v", err)
	}
	if plan.Summary != "fine" || plan.Findings == nil {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestParseClaudePlanResultString(t *testing.T) {
	out := []byte(`{"result":"{\"summary\":\"one issue\",\"findings\":[]}"}`)
	plan, err := parseClaudePlan(out)
	if err != nil {
		t.Fatalf("parseClaudePlan: %v", err)
	}
	if plan.Summary != "one issue" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestParseClaudePlanRequiresFindings(t *testing.T) {
	_, err := parseClaudePlan([]byte(`{"summary":"missing"}`))
	if err == nil || !strings.Contains(err.Error(), "missing findings") {
		t.Fatalf("err = %v", err)
	}
}
