package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A caller that kills its whole process group when it finishes (an agent's
// shell tool, a timed-out command) must not take a started codex run with
// it: the launcher's exit record still has to land.
func TestCodexRunSurvivesCallerGroupKill(t *testing.T) {
	if os.Getenv("AGENTMUX_TEST_DETACH_HELPER") == "1" {
		_, _, err := startCodexProcessDetached(context.Background(), "detach-test", []string{"exec", "--json", "x"}, "prompt", os.Getenv("AGENTMUX_TEST_WORKDIR"), os.Getenv("AGENTMUX_TEST_LOG"))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second) // a long-lived caller; the parent kills its group
		return
	}

	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fakecodex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	// A wrapper, not a symlink: the fake finds its fixtures next to itself.
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexec '"+fake+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "run.jsonl")

	helper := exec.Command(os.Args[0], "-test.run=^TestCodexRunSurvivesCallerGroupKill$")
	helper.Env = append(os.Environ(),
		"AGENTMUX_TEST_DETACH_HELPER=1",
		"AGENTMUX_TEST_WORKDIR="+t.TempDir(),
		"AGENTMUX_TEST_LOG="+logPath,
		"HOME="+home,
		"FAKE_CODEX_SCENARIO=hang",
		"FAKE_CODEX_HANG_SECONDS=2",
	)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer helper.Process.Kill()

	if !waitForLog(logPath, `"agentmux.start"`, 10*time.Second) {
		t.Fatalf("run never started; log: %q", readLog(logPath))
	}
	if err := syscall.Kill(-helper.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()

	if !waitForLog(logPath, `"agentmux.exit"`, 15*time.Second) {
		t.Fatalf("run died with its caller's process group: no exit record; log: %q", readLog(logPath))
	}
}

func waitForLog(path, needle string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(readLog(path), needle) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func readLog(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
