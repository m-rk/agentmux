package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexGuardHarness installs the wrapper in front of the fake codex and
// returns the wrapper path, an env for running it, and the fake's argv log.
func codexGuardHarness(t *testing.T, extra ...string) (wrapper string, env []string, argvLog, taskLog string) {
	t.Helper()
	home := t.TempDir()
	stubDir := filepath.Join(home, ".agentmux", "stubs")
	if err := ensureTaskCodexStub(stubDir); err != nil {
		t.Fatal(err)
	}
	fakeDir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fakecodex"))
	if err != nil {
		t.Fatal(err)
	}
	argvLog = filepath.Join(t.TempDir(), "argv")
	taskLog = filepath.Join(t.TempDir(), "task.log")
	env = append([]string{
		"PATH=" + stubDir + ":" + fakeDir + ":/usr/bin:/bin",
		"HOME=" + home,
		"FAKE_CODEX_ARGV_LOG=" + argvLog,
		"AGENTMUX_TASK_LOG=" + taskLog,
	}, extra...)
	return filepath.Join(stubDir, "codex"), env, argvLog, taskLog
}

var codexExecCalls = [][]string{
	{"exec", "--json", "-"},
	{"exec", "resume", "00000000-0000-4000-8000-000000000001", "--json", "go on"},
	{"-C", ".", "exec", "--json", "hi"},
}

func TestTaskCodexWrapperRefusesLiveExec(t *testing.T) {
	for _, args := range codexExecCalls {
		wrapper, env, argvLog, taskLog := codexGuardHarness(t)
		out, err := runWrapper(t, wrapper, env, args...)
		if err == nil {
			t.Fatalf("wrapper %v ran codex exec without the opt-in", args)
		}
		if !strings.Contains(out, taskCodexStubRefusal) || strings.Count(strings.TrimSpace(out), "\n") != 0 {
			t.Fatalf("refusal = %q, want the one-line text", out)
		}
		if !strings.Contains(out, "fakecodex") {
			t.Fatalf("refusal = %q, want a pointer at the fake", out)
		}
		logged, _ := os.ReadFile(taskLog)
		if !strings.Contains(string(logged), taskCodexStubRefusal) {
			t.Fatalf("task log = %q, want the refusal line", logged)
		}
		if _, serr := os.Stat(argvLog); !os.IsNotExist(serr) {
			t.Fatalf("codex ran despite the refusal (%v)", args)
		}
	}
}

func TestTaskCodexWrapperAllowsWithFlag(t *testing.T) {
	for _, args := range codexExecCalls {
		wrapper, env, argvLog, _ := codexGuardHarness(t, AllowLiveCodexEnv+"=1")
		// The "real" codex behind the wrapper is the fake here: no live call.
		out, err := runWrapper(t, wrapper, env, args...)
		if err != nil {
			t.Fatalf("wrapper %v with the flag: %v: %s", args, err, out)
		}
		if got := readArgv(t, argvLog); strings.Join(got, " ") != strings.Join(args, " ") {
			t.Fatalf("fake saw %q, want %q", got, args)
		}
	}
}

func TestTaskCodexWrapperLeavesOtherCallsAlone(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"login", "status"}} {
		wrapper, env, argvLog, taskLog := codexGuardHarness(t)
		if out, err := runWrapper(t, wrapper, env, args...); err != nil {
			t.Fatalf("wrapper %v: %v: %s", args, err, out)
		}
		if got := readArgv(t, argvLog); strings.Join(got, " ") != strings.Join(args, " ") {
			t.Fatalf("fake saw %q, want %q", got, args)
		}
		if _, serr := os.Stat(taskLog); !os.IsNotExist(serr) {
			t.Fatalf("task log written for a normal call %v", args)
		}
	}
}

func TestTaskAmpStubArgsInstallsCodexWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostModeFile(t, home, "high")
	if got := taskAmpStubArgs("task-9", map[string]string{}); len(got) != 4 {
		t.Fatalf("taskAmpStubArgs = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(home, ".agentmux", "stubs", "codex")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("codex wrapper missing or not executable: %v", err)
	}
}
