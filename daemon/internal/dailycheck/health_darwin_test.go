package dailycheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func TestLaunchdLastExitCode(t *testing.T) {
	if code, ok := launchdLastExitCode("state = not running\nlast exit code = 7\n"); !ok || code != 7 {
		t.Fatalf("launchdLastExitCode = %d, %v", code, ok)
	}
}

func TestLaunchdRunning(t *testing.T) {
	if !launchdRunning("state = running\nlast exit code = 0\n") {
		t.Fatal("running LaunchAgent was not detected")
	}
	if launchdRunning("state = not running\nlast exit code = 0\n") {
		t.Fatal("stopped LaunchAgent was reported as running")
	}
}

// fakeLaunchctl installs a stub `launchctl` on PATH that appends its
// arguments to a log file and exits 0, or exits 1 if failLabel appears in
// its arguments. Returns the log file path.
func fakeLaunchctl(t *testing.T, failLabel string) string {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logFile + "\"\n"
	if failLabel != "" {
		script += "case \"$@\" in *" + failLabel + "*) exit 1;; esac\n"
	}
	script += "exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func TestReloadLaunchdJobBootsOutThenBootstraps(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	logFile := fakeLaunchctl(t, "")
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.agentmux.test.update.plist")

	if err := reloadLaunchdJob(context.Background(), "com.agentmux.test.update"); err != nil {
		t.Fatalf("reloadLaunchdJob: %v", err)
	}

	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 launchctl calls (bootout, bootstrap), got %d: %q", len(lines), logged)
	}
	if !strings.HasPrefix(lines[0], "bootout ") || !strings.Contains(lines[0], plist) {
		t.Fatalf("first call should be bootout with the plist path, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "bootstrap ") || !strings.Contains(lines[1], plist) {
		t.Fatalf("second call should be bootstrap with the plist path, got %q", lines[1])
	}
}

func TestReloadLaunchdJobReturnsErrorWhenBootstrapFails(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	fakeLaunchctl(t, "bootstrap")

	if err := reloadLaunchdJob(context.Background(), "com.agentmux.test.update"); err == nil {
		t.Fatal("expected an error when launchctl bootstrap fails")
	}
}

func hasIssueCode(issues []HealthIssue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

// TestProbeSkipsRefreshChecksWhenAmpUpdateOff guards the self-updating
// host-runner shape: with AGENTMUX_AMP_UPDATE=off in the registry there is
// no update LaunchAgent by design, so Probe must not report refresh-*
// issues for it — while an identical instance without the opt-out still
// gets refresh-missing when its update agent is not loaded.
func TestProbeSkipsRefreshChecksWhenAmpUpdateOff(t *testing.T) {
	dir := t.TempDir()
	prev := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = prev })
	fakeLaunchctl(t, ".update") // main agent loads; update agent fails to print

	inst := &pb.Instance{Name: "probe", Agent: "amp"}
	if got := (platformProber{}).Probe(context.Background(), inst); !hasIssueCode(got, "refresh-missing") {
		t.Fatalf("Probe without opt-out reported %v, want a refresh-missing issue", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte("AGENTMUX_AGENT=amp\nAGENTMUX_AMP_UPDATE=off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, got := range (platformProber{}).Probe(context.Background(), inst) {
		if strings.HasPrefix(got.Code, "refresh-") {
			t.Errorf("Probe with AGENTMUX_AMP_UPDATE=off reported %v, want no refresh-* issues", got)
		}
	}
}
