package dailycheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
