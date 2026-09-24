package threadwatch

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

func TestAmpReviewerUsesStdinAndParsesFencedJSON(t *testing.T) {
	var gotArgs []string
	reviewer := AmpReviewer{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotArgs = append([]string(nil), args...)
			return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf '```json\\n{\"summary\":\"ok\",\"insights\":[]}\\n```'")
		},
		Config: ampexec.Config{Executor: "local", Label: "agentmux-review"},
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
	if !strings.Contains(joined, "-x") || !strings.Contains(joined, "--executor local") || !strings.Contains(joined, "-l agentmux-review") {
		t.Errorf("args missing expected flags: %q", joined)
	}
	if strings.Contains(joined, "secret pane text") {
		t.Fatal("evidence leaked into process arguments instead of stdin")
	}
}

func TestAmpReviewerParsesInsights(t *testing.T) {
	script := `cat >/dev/null; printf '{"summary":"found one","insights":[{"title":"missing allow rule","instances":["a"],"evidence":"ev","suggestion":"allow tool X","kind":"permission"}]}'`
	reviewer := AmpReviewer{Command: fakeAmpCommand(t, script)}
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

func TestAmpReviewerBadOutputFails(t *testing.T) {
	reviewer := AmpReviewer{Command: fakeAmpCommand(t, `cat >/dev/null; printf 'not json at all'`)}
	if _, err := reviewer.Review(context.Background(), ReviewInput{}); err == nil {
		t.Fatal("Review: want error for unparsable amp output, so the caller falls back")
	}
}

func TestAmpReviewerErrorPropagates(t *testing.T) {
	reviewer := AmpReviewer{Command: fakeAmpCommand(t, `cat >/dev/null; echo boom 1>&2; exit 1`)}
	if _, err := reviewer.Review(context.Background(), ReviewInput{}); err == nil {
		t.Fatal("Review: want error from a failing command")
	}
}

func TestAmpReviewerNoCommandFactory(t *testing.T) {
	if _, err := (AmpReviewer{}).Review(context.Background(), ReviewInput{}); err == nil {
		t.Fatal("Review: want error with no Command factory")
	}
}
