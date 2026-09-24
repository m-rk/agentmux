package dailycheck

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/ampexec"
)

func fakeAmpCommand(t *testing.T, script string) ampexec.CommandFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "amp" {
			t.Fatalf("binary = %q, want amp", name)
		}
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

func TestAmpAnalyzerUsesStdinAndParsesFencedJSON(t *testing.T) {
	var gotArgs []string
	analyzer := AmpAnalyzer{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotArgs = append([]string(nil), args...)
			return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf '```json\\n{\"summary\":\"fine\",\"findings\":[]}\\n```'")
		},
		Config: ampexec.Config{Executor: "local", Label: "agentmux-doctor"},
	}
	plan, err := analyzer.Analyze(context.Background(), []Snapshot{{Name: "one", Pane: "secret pane text"}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if plan.Summary != "fine" {
		t.Errorf("Summary = %q, want fine", plan.Summary)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "-x") || !strings.Contains(joined, "--executor local") {
		t.Errorf("args missing expected flags: %q", joined)
	}
	if strings.Contains(joined, "secret pane text") {
		t.Fatal("pane text leaked into process arguments instead of stdin")
	}
}

func TestAmpAnalyzerParsesFindings(t *testing.T) {
	script := `cat >/dev/null; printf '{"summary":"one issue","findings":[{"instance":"one","notable":true,"finding":"dead","evidence":"ev","action":"start","reason":"session-dead"}]}'`
	analyzer := AmpAnalyzer{Command: fakeAmpCommand(t, script)}
	plan, err := analyzer.Analyze(context.Background(), []Snapshot{{Name: "one"}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(plan.Findings) != 1 || plan.Findings[0].Action != ActionStart {
		t.Errorf("Findings = %+v", plan.Findings)
	}
}

func TestAmpAnalyzerBadOutputFails(t *testing.T) {
	analyzer := AmpAnalyzer{Command: fakeAmpCommand(t, `cat >/dev/null; printf 'not json at all'`)}
	if _, err := analyzer.Analyze(context.Background(), nil); err == nil {
		t.Fatal("Analyze: want error for unparsable amp output, so the caller falls back")
	}
}

func TestAmpAnalyzerMissingFindingsFails(t *testing.T) {
	analyzer := AmpAnalyzer{Command: fakeAmpCommand(t, `cat >/dev/null; printf '{"summary":"no findings key"}'`)}
	if _, err := analyzer.Analyze(context.Background(), nil); err == nil {
		t.Fatal("Analyze: want error when findings is missing")
	}
}

func TestAmpAnalyzerErrorPropagates(t *testing.T) {
	analyzer := AmpAnalyzer{Command: fakeAmpCommand(t, `cat >/dev/null; echo boom 1>&2; exit 1`)}
	if _, err := analyzer.Analyze(context.Background(), nil); err == nil {
		t.Fatal("Analyze: want error from a failing command")
	}
}

func TestAmpAnalyzerNoCommandFactory(t *testing.T) {
	if _, err := (AmpAnalyzer{}).Analyze(context.Background(), nil); err == nil {
		t.Fatal("Analyze: want error with no Command factory")
	}
}
