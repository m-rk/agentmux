package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

const codexTestThread = "00000000-0000-4000-8000-0000000000c2"

// newCodexEnv makes a codex instance named "cx" plus a fake HOME whose
// ~/.local/bin/codex runs the fake CLI. That directory leads the run PATH,
// so no real codex can be picked up.
func newCodexEnv(t *testing.T, extra ...string) (home, workdir string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, workdir = filepath.Join(root, "home"), filepath.Join(root, "work")
	bin := filepath.Join(home, ".local", "bin")
	for _, d := range []string{bin, workdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fakecodex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexec "+fake+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("FAKE_CODEX_THREAD_ID", codexTestThread)
	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	lines := append([]string{"AGENTMUX_AGENT=codex", "AGENTMUX_WORKDIR=" + workdir, "AGENTMUX_MODEL=test-model"}, extra...)
	if err := os.WriteFile(filepath.Join(envDir, "cx.env"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, workdir
}

func codexAddr(thread string) string {
	return address.Address{Instance: "cx", Host: address.LocalHostName(), Thread: thread}.String()
}

func waitCodexState(t *testing.T, home, thread, want string) session.CodexRunState {
	t.Helper()
	logPath := session.CodexRunLogPath(home, "cx", thread)
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := session.CodexRunStateOf(logPath)
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(logPath)
			t.Fatalf("state %+v, want %s; log:\n%s", st, want, data)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunCodexStartsFinishesAndContinues(t *testing.T) {
	home, workdir := newCodexEnv(t)
	argvLog := filepath.Join(t.TempDir(), "argv")
	stdinLog := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("FAKE_CODEX_ARGV_LOG", argvLog)
	t.Setenv("FAKE_CODEX_STDIN_LOG", stdinLog)

	res, err := Env{}.Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "do the thing", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != "codex" || res.Thread != codexTestThread || res.State != "running" || res.ThreadURL != "" {
		t.Fatalf("result = %+v", res)
	}
	waitCodexState(t, home, codexTestThread, "done")
	argv, _ := os.ReadFile(argvLog)
	for _, want := range []string{"exec", "--json", "-C\n" + workdir, "-s\nworkspace-write", "-m\ntest-model", "model_reasoning_effort=low"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	if strings.Contains(string(argv), "do the thing") {
		t.Errorf("prompt leaked into argv:\n%s", argv)
	}
	if prompt, _ := os.ReadFile(stdinLog); string(prompt) != "do the thing" {
		t.Errorf("stdin prompt = %q", prompt)
	}
	if matches, _ := filepath.Glob(filepath.Join(session.CodexRunStateDir(home, "cx"), "codex-prompt-*")); len(matches) != 0 {
		t.Errorf("prompt file left behind: %v", matches)
	}
	if _, err := os.Stat(session.CodexRunLogPath(home, "cx", "")); err == nil {
		t.Error("pending log left behind")
	}
	st, err := codexStatus(StatusResult{}, mustAddr(t, codexAddr(codexTestThread)))
	if err != nil || st.State != "done" || st.Run == nil || st.Run.State != "done" {
		t.Fatalf("status = %+v, %v", st, err)
	}

	// Continue: a new turn on the same thread, appended to the same log.
	res, err = Env{}.Run(context.Background(), RunRequest{Address: codexAddr(codexTestThread), Text: "and more", Sandbox: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Thread != codexTestThread {
		t.Fatalf("continue thread = %q", res.Thread)
	}
	waitCodexState(t, home, codexTestThread, "done")
	argv, _ = os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "resume\n"+codexTestThread) || !strings.Contains(string(argv), "-s\nread-only") {
		t.Errorf("resume argv:\n%s", argv)
	}
	data, _ := os.ReadFile(session.CodexRunLogPath(home, "cx", codexTestThread))
	if n := strings.Count(string(data), `"type":"thread.started"`); n != 2 {
		t.Errorf("want two segments in the log, got %d:\n%s", n, data)
	}
}

func mustAddr(t *testing.T, s string) address.Address {
	t.Helper()
	a, err := address.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRunCodexFailureStates(t *testing.T) {
	for scenario, wantRate := range map[string]bool{"turn_failed": false, "rate_limit": true} {
		home, _ := newCodexEnv(t)
		t.Setenv("FAKE_CODEX_SCENARIO", scenario)
		if _, err := (Env{}).Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x"}); err != nil {
			t.Fatalf("%s: %v", scenario, err)
		}
		st := waitCodexState(t, home, codexTestThread, "failed")
		if st.RateLimited != wantRate || st.Reason == "" {
			t.Errorf("%s: %+v", scenario, st)
		}
	}
}

func TestRunCodexContinueRefusesWhileRunning(t *testing.T) {
	home, _ := newCodexEnv(t)
	t.Setenv("FAKE_CODEX_SCENARIO", "hang")
	t.Setenv("FAKE_CODEX_HANG_SECONDS", "30")
	if _, err := (Env{}).Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.StopCodexRuns("cx", "") })
	_, err := Env{}.Run(context.Background(), RunRequest{Address: codexAddr(codexTestThread), Text: "again"})
	if got := AsError(err); got.Reason != safesend.ReasonBusy {
		t.Fatalf("continue of a running thread: %v", err)
	}
	// A fresh (not stalled) run is spared even by a send.
	_, err = Env{}.Run(context.Background(), RunRequest{Address: codexAddr(codexTestThread), Text: "again", InterruptStalled: true})
	if got := AsError(err); got.Reason != safesend.ReasonBusy {
		t.Fatalf("send to a fresh running thread: %v", err)
	}
	// A stalled one is stopped and continued.
	logPath := session.CodexRunLogPath(home, "cx", codexTestThread)
	old := time.Now().Add(-session.CodexRunStalledAfter - time.Minute)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODEX_SCENARIO", "success")
	if _, err := (Env{}).Run(context.Background(), RunRequest{Address: codexAddr(codexTestThread), Text: "nudge", InterruptStalled: true}); err != nil {
		t.Fatalf("send to stalled thread: %v", err)
	}
	waitCodexState(t, home, codexTestThread, "done")
}

func TestRunCodexRefusesUnsafeSandbox(t *testing.T) {
	home, _ := newCodexEnv(t)
	_, err := Env{}.Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x", Sandbox: "danger-full-access"})
	if got := AsError(err); got.Reason != safesend.ReasonInvalid || !strings.Contains(got.Detail, session.CodexUnsafeSandboxEnv) {
		t.Fatalf("danger-full-access without opt-in: %v", err)
	}
	if _, err := os.Stat(session.CodexRunStateDir(home, "cx")); err == nil {
		t.Error("a refused run created state")
	}

	newCodexEnv(t, session.CodexUnsafeSandboxEnv+"=1")
	if _, err := (Env{}).Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x", Sandbox: "danger-full-access", DryRun: true}); err != nil {
		t.Fatalf("opted-in instance refused: %v", err)
	}
}

func TestRunCodexDryRunStartsNothing(t *testing.T) {
	home, _ := newCodexEnv(t)
	res, err := Env{}.Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x", DryRun: true})
	if err != nil || !res.DryRun || res.Agent != "codex" || len(res.Plan) != 1 || !strings.Contains(res.Plan[0], "test-model") {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if _, err := os.Stat(session.CodexRunStateDir(home, "cx")); err == nil {
		t.Error("dry run created state")
	}
}

func TestRunCodexBadLaunchIsRefused(t *testing.T) {
	newCodexEnv(t)
	t.Setenv("FAKE_CODEX_SCENARIO", "nonexistent")
	_, err := Env{}.Run(context.Background(), RunRequest{Address: codexAddr(""), Text: "x"})
	if got := AsError(err); got.Reason != safesend.ReasonFailed || !strings.Contains(got.Detail, "unknown scenario") {
		t.Fatalf("launch failure: %v", err)
	}
}
