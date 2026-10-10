package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// TestStopManagedUsesSystemctlStop is the Linux privilege-split
// contract: stopping the managed half goes through `systemctl stop`, so
// the unit's own ExecStop runs the agent shutdown as the unit user —
// never the caller's. The systemctl invocation is faked; the PATH is
// untouched.
func TestStopManagedUsesSystemctlStop(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemctl stop path is Linux-only")
	}
	var calls [][]string
	old := systemctlStop
	systemctlStop = func(instance string) ([]byte, error) {
		calls = append(calls, []string{"stop", "agentmux-" + instance + ".service"})
		return []byte(""), nil
	}
	t.Cleanup(func() { systemctlStop = old })
	if err := StopManaged("task-9"); err != nil {
		t.Fatalf("StopManaged: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "stop agentmux-task-9.service" {
		t.Errorf("calls = %v, want one systemctl stop of the instance unit", calls)
	}
}

// TestRemoveUnitsDisablesEveryArtifact: with a fake systemctl, removing
// units disables each managed unit and reloads the daemon, and deletes
// the registry entry — without touching /etc (missing files are
// skipped) or the real service manager.
func TestRemoveUnitsDisablesEveryArtifact(t *testing.T) {
	var calls [][]string
	oldSystemctl := systemctl
	systemctl = func(args ...string) {
		calls = append(calls, args)
	}
	t.Cleanup(func() { systemctl = oldSystemctl })
	oldBootout := launchctlBootout
	launchctlBootout = func(label string) {}
	t.Cleanup(func() { launchctlBootout = oldBootout })

	dir := t.TempDir()
	oldEnv := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = oldEnv })
	if err := os.WriteFile(filepath.Join(dir, "task-9.env"), []byte("AGENTMUX_INSTANCE_NAME=task-9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnits("task-9-definitely-not-a-real-instance"); err != nil {
		t.Fatalf("RemoveUnits missing units: %v", err)
	}
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{
		"disable --now agentmux-task-9-definitely-not-a-real-instance.service",
		"disable --now agentmux-task-9-definitely-not-a-real-instance-update.service",
		"disable --now agentmux-task-9-definitely-not-a-real-instance-update.timer",
		"disable --now agentmux-task-9-definitely-not-a-real-instance-tick.service",
		"disable --now agentmux-task-9-definitely-not-a-real-instance-tick.timer",
		"daemon-reload",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemctl calls missing %q:\n%s", want, joined)
		}
	}
	if err := RemoveRegistry("task-9"); err != nil {
		t.Fatalf("RemoveRegistry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-9.env")); !os.IsNotExist(err) {
		t.Error("registry entry still present")
	}
}

func TestMoveIfExistsArchivesFile(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "instance.env")
	to := filepath.Join(dir, "retired", "instance.env")
	if err := os.WriteFile(from, []byte("AGENTMUX_AGENT=codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := moveIfExists(from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	got, err := os.ReadFile(to)
	if err != nil || string(got) != "AGENTMUX_AGENT=codex\n" {
		t.Fatalf("archived file = %q, %v", got, err)
	}
}

func TestMoveIfExistsIgnoresMissingArtifact(t *testing.T) {
	if err := moveIfExists(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "archive")); err != nil {
		t.Fatalf("missing optional artifact: %v", err)
	}
}
