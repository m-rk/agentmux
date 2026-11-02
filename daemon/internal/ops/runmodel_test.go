package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// newModelRunEnv registers one instance of the given agent with extra
// registry lines and returns its address.
func newModelRunEnv(t *testing.T, agent string, extra ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(root, "work")
	envDir := filepath.Join(root, "env")
	for _, d := range []string{workdir, envDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	lines := append([]string{"AGENTMUX_AGENT=" + agent, "AGENTMUX_WORKDIR=" + workdir}, extra...)
	if err := os.WriteFile(filepath.Join(envDir, "inst.env"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "inst@" + address.LocalHostName()
}

// TestRunModelAndEffort covers sessions run -model/-effort for both
// runners as dry runs: accepted, refused and omitted.
func TestRunModelAndEffort(t *testing.T) {
	cases := []struct {
		name          string
		agent         string
		extra         []string
		model, effort string
		wantReason    safesend.Reason // empty: accepted
		wantPlan      string          // substring of the dry-run plan
		notInPlan     string
	}{
		{name: "claude model and effort", agent: "claude-code", model: "opus", effort: "high", wantPlan: "model opus, effort high"},
		{name: "claude full id", agent: "claude-code", model: "claude-model-a-1", wantPlan: "model claude-model-a-1, effort default"},
		{name: "claude effort only", agent: "claude-code", effort: "max", wantPlan: "effort max"},
		{name: "claude omitted", agent: "claude-code", wantPlan: "send prompt", notInPlan: "relaunch"},
		{name: "claude same as pin", agent: "claude-code", extra: []string{"AGENTMUX_CLAUDE_MODEL=opus", "AGENTMUX_CLAUDE_EFFORT=low"}, model: "opus", effort: "low", wantPlan: "send prompt", notInPlan: "relaunch"},
		{name: "claude differs from pin", agent: "claude-code", extra: []string{"AGENTMUX_CLAUDE_MODEL=opus", "AGENTMUX_CLAUDE_EFFORT=low"}, effort: "high", wantPlan: "model opus, effort high"},
		{name: "claude unknown model", agent: "claude-code", model: "model-a", wantReason: safesend.ReasonInvalid},
		{name: "claude unknown effort", agent: "claude-code", effort: "effort-x", wantReason: safesend.ReasonInvalid},
		{name: "claude unknown effort with good model", agent: "claude-code", model: "sonnet", effort: "effort-x", wantReason: safesend.ReasonInvalid},
		{name: "codex model and effort", agent: "codex", model: "model-a", effort: "effort-x", wantPlan: "with model model-a"},
		{name: "codex omitted", agent: "codex", extra: []string{"AGENTMUX_MODEL=model-b"}, wantPlan: "with model model-b"},
		{name: "codex bad model shape", agent: "codex", model: "bad model!", wantReason: safesend.ReasonInvalid},
		{name: "codex bad effort shape", agent: "codex", effort: "Not Ok", wantReason: safesend.ReasonInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := newModelRunEnv(t, tc.agent, tc.extra...)
			res, err := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi", DryRun: true, Model: tc.model, Effort: tc.effort})
			if tc.wantReason != "" {
				wantReason(t, err, tc.wantReason)
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			plan := strings.Join(res.Plan, "\n")
			if !res.OK || !res.DryRun || res.Agent != tc.agent || !strings.Contains(plan, tc.wantPlan) {
				t.Fatalf("result = %+v, want plan containing %q", res, tc.wantPlan)
			}
			if tc.notInPlan != "" && strings.Contains(plan, tc.notInPlan) {
				t.Fatalf("plan %q should not contain %q", plan, tc.notInPlan)
			}
		})
	}
}

// TestRunClaudeRealRun checks the live path: the pin is recorded and the
// session relaunched only when it changes, and a failed delivery restores
// the old pin. Delivery itself is faked.
func TestRunClaudeRealRun(t *testing.T) {
	cases := []struct {
		name          string
		extra         []string
		model, effort string
		deliverErr    error
		wantRelaunch  bool
		wantModel     string
		wantEffort    string
		wantErr       bool
	}{
		{name: "new pin relaunches", model: "opus", effort: "high", wantRelaunch: true, wantModel: "opus", wantEffort: "high"},
		{name: "omitted leaves session alone", extra: []string{"AGENTMUX_CLAUDE_MODEL=opus"}, wantModel: "opus"},
		{name: "unchanged pin leaves session alone", extra: []string{"AGENTMUX_CLAUDE_MODEL=opus"}, model: "opus", wantModel: "opus"},
		{name: "partial change keeps other half", extra: []string{"AGENTMUX_CLAUDE_MODEL=opus", "AGENTMUX_CLAUDE_EFFORT=low"}, effort: "max", wantRelaunch: true, wantModel: "opus", wantEffort: "max"},
		{name: "failed delivery restores pin", extra: []string{"AGENTMUX_CLAUDE_MODEL=opus"}, model: "sonnet", deliverErr: Refuse(safesend.ReasonBusy, "mid-turn"), wantRelaunch: true, wantModel: "opus", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := newModelRunEnv(t, "claude-code", tc.extra...)
			var gotRelaunch, called bool
			old := claudeDeliver
			claudeDeliver = func(_ context.Context, _ Env, instance, text string, relaunch bool) error {
				called, gotRelaunch = true, relaunch
				fields, _ := session.ReadRegistry(instance)
				if relaunch && tc.model != "" && fields[provision.ClaudeModelKey] != tc.model {
					t.Errorf("pin not recorded before relaunch: %v", fields)
				}
				return tc.deliverErr
			}
			t.Cleanup(func() { claudeDeliver = old })
			res, err := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi", Model: tc.model, Effort: tc.effort})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !called || gotRelaunch != tc.wantRelaunch {
				t.Fatalf("called=%v relaunch=%v, want relaunch %v", called, gotRelaunch, tc.wantRelaunch)
			}
			if err == nil && (!res.OK || res.Agent != "claude-code" || res.State != "running") {
				t.Fatalf("result = %+v", res)
			}
			fields, _ := session.ReadRegistry("inst")
			if fields[provision.ClaudeModelKey] != tc.wantModel || fields[provision.ClaudeEffortKey] != tc.wantEffort {
				t.Fatalf("pin = %q/%q, want %q/%q", fields[provision.ClaudeModelKey], fields[provision.ClaudeEffortKey], tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// TestRunClaudeRefusals: an unknown value is refused before any pin is
// written or delivery attempted; a thread and a sandbox make no sense for
// claude.
func TestRunClaudeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  RunRequest
	}{
		{"unknown model", RunRequest{Model: "model-a"}},
		{"unknown effort", RunRequest{Effort: "effort-x"}},
		{"sandbox", RunRequest{Sandbox: "read-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := newModelRunEnv(t, "claude-code")
			old := claudeDeliver
			claudeDeliver = func(context.Context, Env, string, string, bool) error {
				t.Error("delivery attempted")
				return nil
			}
			t.Cleanup(func() { claudeDeliver = old })
			tc.req.Address, tc.req.Text = addr, "hi"
			_, err := Env{}.Run(context.Background(), tc.req)
			wantReason(t, err, safesend.ReasonInvalid)
			fields, _ := session.ReadRegistry("inst")
			if fields[provision.ClaudeModelKey] != "" || fields[provision.ClaudeEffortKey] != "" {
				t.Fatalf("pin written on refusal: %v", fields)
			}
		})
	}
}
