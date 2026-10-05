package daemonserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

// TestRetireInstanceRefusesNonTask mirrors retire's guard: the RPC only
// ever touches task-* instances.
func TestRetireInstanceRefusesNonTask(t *testing.T) {
	for _, name := range []string{"web", "mergentic", "task"} {
		resp, err := New().RetireInstance(context.Background(), &pb.RetireInstanceRequest{Instance: name})
		if err != nil {
			t.Fatalf("RetireInstance(%q): %v", name, err)
		}
		if resp.Ok {
			t.Errorf("RetireInstance(%q) ok, want a refusal", name)
		}
	}
}

// TestRetireInstanceUnknownInstance refuses an unregistered name without
// touching the service manager.
func TestRetireInstanceUnknownInstance(t *testing.T) {
	dir := t.TempDir()
	old := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = old })
	resp, err := New().RetireInstance(context.Background(), &pb.RetireInstanceRequest{Instance: "task-nope"})
	if err != nil {
		t.Fatalf("RetireInstance: %v", err)
	}
	if resp.Ok || !strings.Contains(resp.Message, "no instance") {
		t.Errorf("resp = %+v, want an unknown-instance refusal", resp)
	}
}

// TestRetireInstanceRemovesRegistry: a registered task instance is
// stopped (advisory — systemctl may not exist in test), its units
// removal attempted, and its registry entry deleted.
func TestRetireInstanceRemovesRegistry(t *testing.T) {
	dir := t.TempDir()
	old := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = old })
	if err := os.WriteFile(filepath.Join(dir, "task-9.env"),
		[]byte("AGENTMUX_INSTANCE_NAME=task-9\nAGENTMUX_AGENT=claude-code\nAGENTMUX_WORKDIR=/nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := New().RetireInstance(context.Background(), &pb.RetireInstanceRequest{Instance: "task-9"})
	if err != nil {
		t.Fatalf("RetireInstance: %v", err)
	}
	if !resp.Ok {
		t.Fatalf("resp = %+v, want ok (stop failure is advisory)", resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-9.env")); !os.IsNotExist(err) {
		t.Error("registry entry still present")
	}
}
