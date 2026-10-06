package liveguard

import (
	"os"
	"testing"
)

// neutralCwd moves the test into a plain directory outside any task
// worktree: the developer's own checkout may itself sit under a
// *-worktrees/task-* path (as this repo's task worktrees do), where the
// fallback refusal is correct and would otherwise fail "allowed" tests.
func neutralCwd(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
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
}

func TestRefusalFromTaskInstanceEnv(t *testing.T) {
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv(TaskEnv, "")
	t.Setenv(AllowEnv, "")
	if Allowed() {
		t.Fatal("Allowed = true in a task session without the override")
	}
	if err := Check(); err == nil || err.Error() != Refusal {
		t.Fatalf("Check() = %v, want the refusal %q", err, Refusal)
	}
}

func TestAllowedFromNormalSession(t *testing.T) {
	neutralCwd(t)
	t.Setenv(InstanceEnv, "site-amp")
	t.Setenv(AllowEnv, "")
	if !Allowed() {
		t.Fatal("Allowed = false outside a task session")
	}
	if err := Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

func TestAllowedWithOverride(t *testing.T) {
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv(TaskEnv, "")
	t.Setenv(AllowEnv, "1")
	if !Allowed() {
		t.Fatal("Allowed = false in a task session with AGENTMUX_ALLOW_LIVE=1")
	}
	if err := Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

func TestOverrideNeedsExactOne(t *testing.T) {
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv(TaskEnv, "")
	for _, v := range []string{"true", "yes", "0", " 1"} {
		t.Setenv(AllowEnv, v)
		if Allowed() {
			t.Fatalf("Allowed = true with AGENTMUX_ALLOW_LIVE=%q", v)
		}
	}
}

func TestNonTaskPrefixAllowed(t *testing.T) {
	neutralCwd(t)
	t.Setenv(InstanceEnv, "mytask-1")
	t.Setenv(TaskEnv, "")
	t.Setenv(AllowEnv, "")
	if !Allowed() {
		t.Fatal("Allowed = false for a name that merely contains task-")
	}
}
