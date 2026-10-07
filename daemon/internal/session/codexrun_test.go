package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCodexThread = "00000000-0000-4000-8000-0000000000c1"

func writeCodexLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex-run-"+testCodexThread+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	evStart    = `{"type":"agentmux.start"}`
	evThread   = `{"type":"thread.started","thread_id":"` + testCodexThread + `"}`
	evTurn     = `{"type":"turn.started"}`
	evDone     = `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`
	evExit0    = `{"type":"agentmux.exit","code":0}`
	evExit1    = `{"type":"agentmux.exit","code":1}`
	evRateErr  = `{"type":"error","message":"{\"type\":\"error\",\"status\":429,\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}"}`
	evRateFail = `{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":429,\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}"}}`
	evBadFail  = `{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad model\"}}"}}`
)

func TestCodexRunStateOf(t *testing.T) {
	cases := []struct {
		name        string
		lines       []string
		state       string
		reasonHas   string
		rateLimited bool
	}{
		{"success", []string{evStart, evThread, evTurn, evDone, evExit0}, "done", "", false},
		{"success without exit record yet", []string{evStart, evThread, evTurn, evDone}, "done", "", false},
		{"running", []string{evStart, evThread, evTurn}, "running", "", false},
		{"turn failed", []string{evStart, evThread, evTurn, evBadFail, evExit1}, "failed", "invalid_request_error: bad model (status 400)", false},
		{"rate limit", []string{evStart, evThread, evTurn, evRateErr, evRateFail, evExit1}, "failed", "rate_limit_error: slow down (status 429)", true},
		{"non-fatal error item is not failure", []string{evStart, evThread, evTurn,
			`{"type":"item.completed","item":{"type":"error","message":"synthetic"}}`, evDone, evExit0}, "done", "", false},
		{"transient error while running", []string{evStart, evThread, evTurn, `{"type":"error","message":"Reconnecting... 1/5"}`}, "running", "", false},
		{"process gone after error", []string{evStart, evThread, evTurn, `{"type":"error","message":"stream closed"}`, evExit1}, "failed", "process exited (code 1) without a result: stream closed", false},
		{"process gone, no events", []string{evStart, "codex: command not found", evExit1}, "failed", "command not found", false},
		{"stale exit of a killed older run is ignored", []string{`{"type":"agentmux.start","run":"a"}`, evThread, evTurn,
			`{"type":"agentmux.start","run":"b"}`, evThread, evTurn, `{"type":"agentmux.exit","run":"a","code":137}`}, "running", "", false},
		{"resume after done is running", []string{evStart, evThread, evTurn, evDone, evExit0, evStart, evThread, evTurn}, "running", "", false},
		{"resume after failure succeeds", []string{evStart, evThread, evBadFail, evExit1, evStart, evThread, evTurn, evDone, evExit0}, "done", "", false},
		{"resume after success fails", []string{evStart, evThread, evTurn, evDone, evExit0, evStart, evTurn, evBadFail, evExit1}, "failed", "bad model", false},
		{"resume dies before thread.started", []string{evStart, evThread, evTurn, evDone, evExit0, evStart, "codex: no session", evExit1}, "failed", "no session", false},
	}
	for _, tc := range cases {
		st := CodexRunStateOf(writeCodexLog(t, tc.lines...))
		if st.State != tc.state || !strings.Contains(st.Reason, tc.reasonHas) || st.RateLimited != tc.rateLimited {
			t.Errorf("%s: got %+v, want state %q reason ~%q rate %v", tc.name, st, tc.state, tc.reasonHas, tc.rateLimited)
		}
		if tc.state != "failed" && st.Reason != "" {
			t.Errorf("%s: unexpected reason %q", tc.name, st.Reason)
		}
		if st.ThreadID != testCodexThread && !strings.Contains(tc.name, "no events") {
			t.Errorf("%s: thread id %q", tc.name, st.ThreadID)
		}
	}
}

func TestCodexRunStateMissingLogIsRunning(t *testing.T) {
	st := CodexRunStateOf(filepath.Join(t.TempDir(), "absent.jsonl"))
	if st.State != "running" || st.Stalled {
		t.Fatalf("missing log: %+v", st)
	}
}

func TestCodexRunStateHangIsStalled(t *testing.T) {
	p := writeCodexLog(t, evStart, evThread, evTurn)
	if st := CodexRunStateOf(p); st.State != "running" || st.Stalled {
		t.Fatalf("fresh hang log: %+v", st)
	}
	old := time.Now().Add(-CodexRunStalledAfter - time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if st := CodexRunStateOf(p); st.State != "running" || !st.Stalled {
		t.Fatalf("quiet log should be running+stalled: %+v", st)
	}
}

func TestCodexRunStateOfFakeFixtures(t *testing.T) {
	for scenario, want := range map[string]string{"success": "done", "turn_failed": "failed", "rate_limit": "failed", "error_item": "done", "hang": "running"} {
		out, _ := runFakeCodex(t, []string{"FAKE_CODEX_SCENARIO=" + scenario, "FAKE_CODEX_HANG_SECONDS=0"}, "exec", "--json", "x")
		p := filepath.Join(t.TempDir(), "log.jsonl")
		if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
		st := CodexRunStateOf(p)
		if st.State != want {
			t.Errorf("%s: state %q, want %q", scenario, st.State, want)
		}
		if scenario == "rate_limit" && !st.RateLimited {
			t.Errorf("rate_limit fixture not flagged rate limited: %+v", st)
		}
	}
}

func TestCodexRunArgs(t *testing.T) {
	got, err := CodexRunArgs(CodexRunOptions{Workdir: "/w", Model: "some-model", Effort: "high"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "exec --json -C /w -s workspace-write -m some-model -c model_reasoning_effort=high -"
	if strings.Join(got, " ") != want {
		t.Errorf("new args = %q, want %q", strings.Join(got, " "), want)
	}
	got, err = CodexRunArgs(CodexRunOptions{Workdir: "/w", Sandbox: "read-only"}, testCodexThread)
	if err != nil {
		t.Fatal(err)
	}
	want = "exec --json -C /w -s read-only resume " + testCodexThread + " -"
	if strings.Join(got, " ") != want {
		t.Errorf("resume args = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestCodexRunArgsRefusals(t *testing.T) {
	bad := []CodexRunOptions{
		{Sandbox: "danger-full-access"},
		{Sandbox: "bogus"},
		{Model: "--dangerously-bypass-approvals-and-sandbox"},
		{Model: "m\nx"},
		{Effort: "high -s danger-full-access"},
	}
	for _, o := range bad {
		if args, err := CodexRunArgs(o, ""); err == nil {
			t.Errorf("%+v accepted: %v", o, args)
		}
	}
	if _, err := CodexRunArgs(CodexRunOptions{}, "not-a-thread"); err == nil {
		t.Error("bad thread id accepted")
	}
	if args, err := CodexRunArgs(CodexRunOptions{Sandbox: "danger-full-access", AllowUnsafe: true}, ""); err != nil || !strings.Contains(strings.Join(args, " "), "danger-full-access") {
		t.Errorf("explicitly allowed danger-full-access: %v %v", args, err)
	}
}

func TestCheckCodexArgsCatchesBypass(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "--dangerously-bypass-approvals-and-sandbox", "-"},
		{"exec", "--yolo", "-"},
		{"exec", "-s", "danger-full-access", "-"},
		{"exec", "--sandbox=danger-full-access", "-"},
		{"exec", "-c", "sandbox_mode=danger-full-access", "-"},
		{"exec", "-c", `sandbox_mode="danger-full-access"`, "-"},
	} {
		if err := CheckCodexArgs(args, false); err == nil {
			t.Errorf("%v not refused", args)
		}
		if err := CheckCodexArgs(args, true); err != nil {
			t.Errorf("%v refused despite opt-in: %v", args, err)
		}
	}
	if err := CheckCodexArgs([]string{"exec", "-s", "workspace-write", "-"}, false); err != nil {
		t.Errorf("safe args refused: %v", err)
	}
}

func TestCodexRunCmdlineMatch(t *testing.T) {
	dir := t.TempDir()
	write := func(args ...string) string {
		p := filepath.Join(dir, "cmdline")
		if err := os.WriteFile(p, []byte(strings.Join(args, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if !codexRunCmdlineMatch(write("/usr/bin/codex", "exec", "--json", "-")) {
		t.Error("codex exec not matched")
	}
	if !codexRunCmdlineMatch(write("node", "/lib/bin/codex", "exec", "-")) {
		t.Error("node-wrapped codex exec not matched")
	}
	if codexRunCmdlineMatch(write("/usr/bin/codex", "login", "status")) || codexRunCmdlineMatch(write("sh", "-c", "codex exec")) {
		t.Error("non-run process matched")
	}
}
