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
	// Clear the identity the guard reads so the suite is hermetic even
	// when run from inside a task session (which exports exactly these).
	// AGENTMUX_ALLOW_LIVE is dead (AMUX-39 removed the override) but a
	// stray export from an older shell is cleared too.
	t.Setenv(TaskEnv, "")
	t.Setenv(InstanceEnv, "")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "")
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
	t.Setenv("AGENTMUX_ALLOW_LIVE", "")
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
	if !Allowed() {
		t.Fatal("Allowed = false outside a task session")
	}
	if err := Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

// TestNoOverrideInTaskSession pins AMUX-39: the old AGENTMUX_ALLOW_LIVE=1
// override does nothing in a task session — there is no self-serve way
// back to live.
func TestNoOverrideInTaskSession(t *testing.T) {
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv(TaskEnv, "")
	t.Setenv("AGENTMUX_ALLOW_LIVE", "1")
	if Allowed() {
		t.Fatal("Allowed = true in a task session with AGENTMUX_ALLOW_LIVE=1: the override is gone")
	}
	if err := Check(); err == nil || err.Error() != Refusal {
		t.Fatalf("Check() = %v, want the refusal %q", err, Refusal)
	}
}

func TestOverrideValuesAllRefused(t *testing.T) {
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv(TaskEnv, "")
	for _, v := range []string{"1", "true", "yes", "0", " 1"} {
		t.Setenv("AGENTMUX_ALLOW_LIVE", v)
		if Allowed() {
			t.Fatalf("Allowed = true with AGENTMUX_ALLOW_LIVE=%q", v)
		}
	}
}

func TestNonTaskPrefixAllowed(t *testing.T) {
	neutralCwd(t)
	t.Setenv(InstanceEnv, "mytask-1")
	t.Setenv(TaskEnv, "")
	if !Allowed() {
		t.Fatal("Allowed = false for a name that merely contains task-")
	}
}
