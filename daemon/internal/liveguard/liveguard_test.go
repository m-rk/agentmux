package liveguard

import (
	"testing"
)

func TestRefusalFromTaskInstanceEnv(t *testing.T) {
	t.Setenv(InstanceEnv, "task-17")
	t.Setenv(AllowEnv, "")
	if Allowed() {
		t.Fatal("Allowed = true in a task session without the override")
	}
	if err := Check(); err == nil || err.Error() != Refusal {
		t.Fatalf("Check() = %v, want the refusal %q", err, Refusal)
	}
}

func TestAllowedFromNormalSession(t *testing.T) {
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
	for _, v := range []string{"true", "yes", "0", " 1"} {
		t.Setenv(AllowEnv, v)
		if Allowed() {
			t.Fatalf("Allowed = true with AGENTMUX_ALLOW_LIVE=%q", v)
		}
	}
}

func TestNonTaskPrefixAllowed(t *testing.T) {
	t.Setenv(InstanceEnv, "mytask-1")
	t.Setenv(AllowEnv, "")
	if !Allowed() {
		t.Fatal("Allowed = false for a name that merely contains task-")
	}
}
