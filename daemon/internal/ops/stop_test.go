package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

func stopReq(addr string) StopRequest {
	return StopRequest{Address: addr, Via: "relayed", By: "orchestrator", From: "MERG-94"}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A live codex run is cancelled: the run log gets the stop and the
// launcher's own exit record, status reads stopped, the instance, worktree
// and branch survive, and a following run resumes on the same thread.
func TestStopCodexCancelsLiveRunAndResumes(t *testing.T) {
	home, workdir := newCodexEnv(t)
	gitIn(t, workdir, "init", "-q", "-b", "task/x")
	gitIn(t, workdir, "commit", "-q", "--allow-empty", "-m", "base")
	t.Setenv("FAKE_CODEX_SCENARIO", "hang")
	t.Setenv("FAKE_CODEX_HANG_SECONDS", "60")
	if _, err := (Env{}).Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "work"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.StopCodexRuns("cx", "") })
	logPath := session.CodexRunLogPath(home, "cx", codexTestThread)
	waitCodexState(t, home, codexTestThread, "running")

	dry, err := Env{}.Stop(context.Background(), StopRequest{Address: codexAddr(""), Via: "relayed", By: "orchestrator", DryRun: true})
	if err != nil || !dry.DryRun || dry.Stopped || dry.State != "running" || len(dry.Plan) != 1 || dry.Thread != codexTestThread {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if st := session.CodexRunStateOf(logPath); st.State != "running" {
		t.Fatalf("dry run disturbed the run: %+v", st)
	}

	res, err := Env{}.Stop(context.Background(), stopReq(codexAddr("")))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Stopped || res.State != "stopped" || res.Thread != codexTestThread || res.Agent != "codex" || res.StoppedAt == nil {
		t.Fatalf("stop = %+v", res)
	}
	log, _ := os.ReadFile(logPath)
	stopAt := strings.Index(string(log), `"type":"agentmux.stop"`)
	exitAt := strings.LastIndex(string(log), `"type":"agentmux.exit"`)
	if stopAt < 0 || exitAt < stopAt {
		t.Fatalf("log lacks a stop followed by the launcher's exit record:\n%s", log)
	}
	st := session.CodexRunStateOf(logPath)
	if st.State != "stopped" || st.StoppedBy != "orchestrator" || st.StoppedAt == "" || st.Reason != "stopped by orchestrator" {
		t.Fatalf("state after stop = %+v", st)
	}
	status, err := codexStatus(StatusResult{}, mustAddr(t, codexAddr(codexTestThread)))
	if err != nil || status.State != "stopped" || status.Run == nil || status.Run.StoppedBy != "orchestrator" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("worktree gone: %v", err)
	}
	if b := gitIn(t, workdir, "branch", "--show-current"); b != "task/x" {
		t.Fatalf("branch = %q", b)
	}
	if _, err := session.ReadRegistry("cx"); err != nil {
		t.Fatalf("instance gone: %v", err)
	}
	audit, _ := os.ReadFile(safesend.AuditPath(home))
	if !strings.Contains(string(audit), `"outcome":"stop:stopped"`) || !strings.Contains(string(audit), `"principal":"orchestrator"`) {
		t.Fatalf("audit = %s", audit)
	}

	// Stopping again is a no-op that reports the finished state.
	again, err := Env{}.Stop(context.Background(), stopReq(codexAddr(codexTestThread)))
	if err != nil || again.Stopped || again.State != "stopped" {
		t.Fatalf("second stop = %+v, %v", again, err)
	}

	argvLog := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_CODEX_ARGV_LOG", argvLog)
	t.Setenv("FAKE_CODEX_SCENARIO", "success")
	run, err := Env{}.Run(context.Background(), RunRequest{Address: codexAddr(codexTestThread), Text: "carry on"})
	if err != nil || run.Thread != codexTestThread {
		t.Fatalf("continue after stop = %+v, %v", run, err)
	}
	waitCodexState(t, home, codexTestThread, "done")
	if argv, _ := os.ReadFile(argvLog); !strings.Contains(string(argv), "resume\n"+codexTestThread) {
		t.Fatalf("continue did not resume the thread:\n%s", argv)
	}
}

func TestStopIdleInstanceIsANoOp(t *testing.T) {
	home, _ := newCodexEnv(t)
	res, err := Env{}.Stop(context.Background(), stopReq(codexAddr("")))
	if err != nil || !res.OK || res.Stopped || res.State != "idle" {
		t.Fatalf("idle stop = %+v, %v", res, err)
	}
	t.Setenv("FAKE_CODEX_SCENARIO", "success")
	if _, err := (Env{}).Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	waitCodexState(t, home, codexTestThread, "done")
	res, err = Env{}.Stop(context.Background(), stopReq(codexAddr("")))
	if err != nil || res.Stopped || res.State != "done" {
		t.Fatalf("stop of a finished run = %+v, %v", res, err)
	}
	if st := session.CodexRunStateOf(session.CodexRunLogPath(home, "cx", codexTestThread)); st.State != "done" {
		t.Fatalf("finished run now %+v", st)
	}
}

func TestStopRefusals(t *testing.T) {
	newCodexEnv(t)
	for name, req := range map[string]StopRequest{
		"no principal": {Address: codexAddr(""), Via: "relayed"},
		"bad via":      {Address: codexAddr(""), Via: "approved", By: "orchestrator"},
		"bad thread":   stopReq(codexAddr("T-nope")),
	} {
		if _, err := (Env{}).Stop(context.Background(), req); AsError(err).Reason != safesend.ReasonInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := (Env{}).Stop(context.Background(), stopReq(codexAddr("00000000-0000-4000-8000-0000000000ff"))); AsError(err).Reason != safesend.ReasonNotFound {
		t.Errorf("unknown thread: %v", err)
	}
	if _, err := (Env{}).Stop(context.Background(), stopReq("nope@"+mustAddr(t, codexAddr("")).Host)); AsError(err).Reason != safesend.ReasonNotFound {
		t.Errorf("unknown instance: %v", err)
	}
	if _, err := (Env{}).Stop(context.Background(), stopReq("cx@elsewhere")); AsError(err).Reason != safesend.ReasonNotLocal {
		t.Errorf("other host: %v", err)
	}
}

func TestStopClaudeInterruptsOnlyABusyTurn(t *testing.T) {
	newCodexEnv(t, "AGENTMUX_AGENT=claude-code")
	oldState, oldKeys := claudePaneState, claudeStopKeys
	t.Cleanup(func() { claudePaneState, claudeStopKeys = oldState, oldKeys })
	pane := safesend.StateReady
	escapes := 0
	claudePaneState = func(context.Context, Env, string) (safesend.State, error) { return pane, nil }
	claudeStopKeys = func(context.Context, Env, string) error { escapes++; pane = safesend.StateReady; return nil }

	addr := codexAddr("")
	for _, idle := range []safesend.State{safesend.StateReady, safesend.StatePrompt, safesend.StateDraft} {
		pane = idle
		res, err := Env{}.Stop(context.Background(), stopReq(addr))
		if err != nil || res.Stopped || res.State != "idle" || escapes != 0 {
			t.Fatalf("%s pane: %+v, %v, escapes %d", idle, res, err, escapes)
		}
	}
	pane = safesend.StateBusy
	res, err := Env{}.Stop(context.Background(), stopReq(addr))
	if err != nil || !res.Stopped || res.State != "stopped" || escapes != 1 || res.Agent != "claude-code" {
		t.Fatalf("busy pane: %+v, %v, escapes %d", res, err, escapes)
	}
	if _, err := (Env{}).Stop(context.Background(), stopReq(codexAddr(codexTestThread))); AsError(err).Reason != safesend.ReasonInvalid {
		t.Fatalf("claude thread address: %v", err)
	}
}
