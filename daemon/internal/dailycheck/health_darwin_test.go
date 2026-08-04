package dailycheck

import "testing"

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
