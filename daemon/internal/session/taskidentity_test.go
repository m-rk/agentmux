package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/liveguard"
)

// TestAmpRunEnvStampsTaskIdentity pins what every amp run child carries:
// the instance name always, plus AGENTMUX_TASK_SESSION=1 for task-*
// instances — the pair the live guard fires on inside the agent's own
// run (see liveguard). Non-task instances name themselves without the
// flag, so every run child is identifiable, not just task ones.
func TestAmpRunEnvStampsTaskIdentity(t *testing.T) {
	task := AmpRunEnv("task-9")
	if !contains(task, liveguard.InstanceEnv+"=task-9") {
		t.Errorf("task env = %q, want the instance name", task)
	}
	if !contains(task, liveguard.TaskEnv+"=1") {
		t.Errorf("task env = %q, want the task flag", task)
	}
	plain := AmpRunEnv("site-amp")
	if !contains(plain, liveguard.InstanceEnv+"=site-amp") {
		t.Errorf("plain env = %q, want the instance name", plain)
	}
	for _, e := range plain {
		if strings.HasPrefix(e, liveguard.TaskEnv+"=") {
			t.Errorf("plain env = %q, want no task flag", plain)
		}
	}
}

// TestAmpRunCommandCarriesTaskIdentityInEnv pins that the identity rides
// the child environment, never argv: with no op env-file the command is
// plain `amp`, and the pair is in Env only.
func TestAmpRunCommandCarriesTaskIdentityInEnv(t *testing.T) {
	cmd, err := ampRunCommandFor(context.Background(), "task-9", "", []string{"-x", "hi"})
	if err != nil {
		t.Fatalf("ampRunCommandFor: %v", err)
	}
	for _, a := range cmd.Args {
		if strings.HasPrefix(a, "AGENTMUX_") {
			t.Fatalf("identity leaked into argv: %q", cmd.Args)
		}
	}
	if !contains(cmd.Env, liveguard.InstanceEnv+"=task-9") {
		t.Errorf("child env missing instance name: %q", cmd.Env)
	}
	if !contains(cmd.Env, liveguard.TaskEnv+"=1") {
		t.Errorf("child env missing task flag: %q", cmd.Env)
	}
}

// TestTaskSessionEnvArgs pins the tmux -e pair task claude-code panes
// get, and that non-task instances get nothing.
func TestTaskSessionEnvArgs(t *testing.T) {
	got := taskSessionEnvArgs("task-9")
	want := []string{"-e", liveguard.InstanceEnv + "=task-9", "-e", liveguard.TaskEnv + "=1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("taskSessionEnvArgs(task-9) = %q, want %q", got, want)
	}
	if got := taskSessionEnvArgs("site-amp"); len(got) != 0 {
		t.Errorf("taskSessionEnvArgs(site-amp) = %q, want nothing", got)
	}
}

// TestRunClaudeCodeStampsTaskEnv pins the actual tmux invocation for a
// task instance: the -e pair lands before the claude command.
func TestRunClaudeCodeStampsTaskEnv(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	registryFile := "" +
		"AGENTMUX_INSTANCE_NAME=task-9\n" +
		"AGENTMUX_TMUX_SESSION_NAME=task-9\n" +
		"AGENTMUX_WORKDIR=" + workdir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "task-9.env"), []byte(registryFile), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	prevWithPath := withPath
	withPath = func(name string, args ...string) *exec.Cmd {
		calls = append(calls, args)
		for _, a := range args {
			if a == "has-session" {
				return exec.Command("false") // not running yet
			}
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { withPath = prevWithPath })
	// prepareClaudeAllowSettings resolves the home for a settings path;
	// point it at a temp home so the test never touches the real one.
	t.Setenv("HOME", t.TempDir())
	if err := RunClaudeCode("task-9"); err != nil {
		t.Fatalf("RunClaudeCode: %v", err)
	}
	var launch []string
	for _, args := range calls {
		for _, a := range args {
			if a == "new-session" {
				launch = args
			}
		}
	}
	if launch == nil {
		t.Fatalf("RunClaudeCode never issued a tmux new-session; calls: %v", calls)
	}
	joined := strings.Join(launch, " ")
	for _, want := range []string{
		"-e " + liveguard.InstanceEnv + "=task-9",
		"-e " + liveguard.TaskEnv + "=1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("launch missing %q: %v", want, launch)
		}
	}
	// The pair must precede the claude command, not ride it.
	claudeAt := -1
	for i, a := range launch {
		if a == "claude" {
			claudeAt = i
			break
		}
	}
	if claudeAt < 0 {
		t.Fatalf("launch has no claude command: %v", launch)
	}
	for _, flag := range []string{liveguard.InstanceEnv + "=task-9", liveguard.TaskEnv + "=1"} {
		found := -1
		for i, a := range launch {
			if a == flag {
				found = i
				break
			}
		}
		if found < 0 || found > claudeAt {
			t.Errorf("flag %q at %d, claude at %d: %v", flag, found, claudeAt, launch)
		}
	}
}

// TestRunClaudeCodeLeavesNonTaskEnvAlone pins that a non-task instance
// gets no -e pair at all.
func TestRunClaudeCodeLeavesNonTaskEnvAlone(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	registryFile := "" +
		"AGENTMUX_INSTANCE_NAME=site-claude\n" +
		"AGENTMUX_TMUX_SESSION_NAME=site-claude\n" +
		"AGENTMUX_WORKDIR=" + workdir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "site-claude.env"), []byte(registryFile), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	prevWithPath := withPath
	withPath = func(name string, args ...string) *exec.Cmd {
		calls = append(calls, args)
		for _, a := range args {
			if a == "has-session" {
				return exec.Command("false") // not running yet
			}
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { withPath = prevWithPath })
	t.Setenv("HOME", t.TempDir())
	if err := RunClaudeCode("site-claude"); err != nil {
		t.Fatalf("RunClaudeCode: %v", err)
	}
	for _, args := range calls {
		for _, a := range args {
			if a == "-e" || strings.HasPrefix(a, liveguard.TaskEnv+"=") {
				t.Fatalf("non-task launch carries task env: %v", args)
			}
		}
	}
}

// TestStopAmpRunsKillsOnlyItsInstance spawns two sleepers shaped like amp
// run children — one stamped for task-9, one for task-10 — in the same
// workdir, then asserts StopAmpRuns kills only task-9's. The sleepers are
// real processes (not fakes) so the /proc scan runs for real; they `exec
// -a` into the amp argv shape with the stamped environ entry.
func TestStopAmpRunsKillsOnlyItsInstance(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc (non-Linux)")
	}
	workdir := t.TempDir()
	spawn := func(instance string) *os.Process {
		t.Helper()
		// exec -a fakes argv[0] as amp with the run shape; the loop
		// keeps that argv alive (a plain `sleep` would re-exec and
		// lose it). The environ entry is what scopes the kill.
		cmd := exec.Command("bash", "-c", `exec -a amp bash -c 'while true; do sleep 1; done' amp --stream-json -x hello`)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(), liveguard.InstanceEnv+"="+instance)
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			t.Fatalf("spawn %s: %v", instance, err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd.Process
	}
	keep := spawn("task-10")
	_ = keep
	victim := spawn("task-9")
	got := stopAmpRunsForTest("task-9", workdir)
	found := false
	for _, pid := range got {
		if pid == victim.Pid {
			found = true
		}
		if pid == keep.Pid {
			t.Fatalf("scan matched task-10's run: %v", got)
		}
	}
	if !found {
		t.Fatalf("scan missed task-9's run (pid %d): %v", victim.Pid, got)
	}
	StopAmpRuns("task-9", workdir)
	for i := 0; i < 20; i++ {
		if !alive(victim.Pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive(victim.Pid) {
		t.Fatalf("task-9's run (pid %d) still alive after StopAmpRuns", victim.Pid)
	}
	if !alive(keep.Pid) {
		t.Fatal("task-10's run died: StopAmpRuns killed outside its instance")
	}
}

// alive reports whether pid still has a live process behind it: the
// /proc entry exists and the process is not a zombie. A SIGKILLed child
// of this test binary lingers as a zombie until reaped, so state "Z"
// counts as dead.
func alive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// stat fields: pid (comm) state ... — the state is the first field
	// after the closing paren of the comm.
	rest := string(data)
	if i := strings.LastIndex(rest, ")"); i >= 0 {
		rest = strings.TrimSpace(rest[i+1:])
		if strings.HasPrefix(rest, "Z") {
			return false
		}
	}
	return true
}
