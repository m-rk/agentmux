package liveguard

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRefusalFromTaskFlag fires on the dedicated flag alone: this is the
// identity `sessions run` stamps on every amp run child of a task-*
// instance (see session.AmpRunEnv), so the guard no longer depends on the
// child happening to inherit the instance name.
func TestRefusalFromTaskFlag(t *testing.T) {
	t.Setenv(TaskEnv, "1")
	t.Setenv(InstanceEnv, "")
	if Allowed() {
		t.Fatal("Allowed = true with AGENTMUX_TASK_SESSION=1 and no override")
	}
	if err := Check(); err == nil || err.Error() != Refusal {
		t.Fatalf("Check() = %v, want the refusal %q", err, Refusal)
	}
}

// TestTaskFlagValuesOtherThanOne fires only on exactly "1": any other
// value is not a task session by itself.
func TestTaskFlagValuesOtherThanOne(t *testing.T) {
	neutralCwd(t)
	t.Setenv(InstanceEnv, "")
	for _, v := range []string{"true", "yes", "0", " 1", "2"} {
		t.Setenv(TaskEnv, v)
		if !Allowed() {
			t.Fatalf("Allowed = false with AGENTMUX_TASK_SESSION=%q outside a task session", v)
		}
	}
}

// TestRefusalFromTaskWorktreeCwd fires with no task environment at all
// when the working directory sits inside a task worktree: the fallback
// for real amp task runs, which carry neither variable.
func TestRefusalFromTaskWorktreeCwd(t *testing.T) {
	t.Setenv(TaskEnv, "")
	t.Setenv(InstanceEnv, "")
	dir := filepath.Join(t.TempDir(), "work", "agentmux-worktrees", "task-AMUX-34")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	})
	if Allowed() {
		t.Fatal("Allowed = true inside a task worktree without the override")
	}
	if err := Check(); err == nil || err.Error() != Refusal {
		t.Fatalf("Check() = %v, want the refusal %q", err, Refusal)
	}
}

// TestAllowedOutsideTaskWorktreeCwd pins the fallback's scope: a plain
// checkout with no task markers stays allowed.
func TestAllowedOutsideTaskWorktreeCwd(t *testing.T) {
	neutralCwd(t)
	t.Setenv(TaskEnv, "")
	t.Setenv(InstanceEnv, "site-amp")
	if !Allowed() {
		t.Fatal("Allowed = false in a plain checkout outside a task session")
	}
}

// TestInTaskWorktree pins the path shapes the fallback fires on.
func TestInTaskWorktree(t *testing.T) {
	for _, dir := range []string{
		"/home/alice/agentmux-worktrees/task-AMUX-34",
		"/home/alice/agentmux-worktrees/task-merg-52/sub",
	} {
		if !InTaskWorktree(dir) {
			t.Errorf("InTaskWorktree(%q) = false, want true", dir)
		}
	}
	for _, dir := range []string{
		"/home/alice/agentmux",
		"/home/alice/agentmux-worktrees/other",
		"/home/alice/task-9",
		"",
	} {
		if InTaskWorktree(dir) {
			t.Errorf("InTaskWorktree(%q) = true, want false", dir)
		}
	}
	// A directory merely ending in -worktrees without a task-* entry
	// stays allowed.
	if InTaskWorktree("/home/alice/agentmux-worktrees/other/task") {
		t.Error("InTaskWorktree(worktrees/other/task) = true, want false")
	}
}

// TestNoOverrideInsideTaskWorktree pins AMUX-39: even the old override
// cannot opt a task worktree back to live — the refusal wins over every
// signal.
func TestNoOverrideInsideTaskWorktree(t *testing.T) {
	t.Setenv(TaskEnv, "1")
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "1")
	dir := filepath.Join(t.TempDir(), "x-worktrees", "task-9")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	})
	if Allowed() {
		t.Fatal("Allowed = true with the override inside a task session: the override is gone")
	}
}
