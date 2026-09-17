//go:build linux

package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestUpdateAmpLocksAcrossInstancesSharingHome reproduces a real incident on
// a Linux host: two amp instances under the same run user's HOME had their
// nightly update timers land within a minute of each other, and both failed
// with `npm error EEXIST: file already exists:
// /home/dev/.npm-global/bin/amp` — `amp update` shells out through npm
// under the hood, hitting the same shared ~/.npm-global prefix opencode's
// install does, but amp (a newer runner type) never got wired into
// withNpmGlobalLock the way opencode/kilo/zero are in updateAgent. This
// guards against the same race recurring.
func TestUpdateAmpLocksAcrossInstancesSharingHome(t *testing.T) {
	envDir := withEnvDir(t)
	withFakeUserLookup(t) // both instances resolve to the same fake home

	for _, name := range []string{"amp1", "amp2"} {
		content := "AGENTMUX_INSTANCE_NAME=" + name + "\nAGENTMUX_AGENT=amp\nAGENTMUX_RUN_USER=someuser\n"
		if err := os.WriteFile(filepath.Join(envDir, name+".env"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var concurrent, maxConcurrent int32
	previousRunAs := runAs
	runAs = func(runUser, name string, args ...string) *exec.Cmd {
		if name == "amp" && len(args) > 0 && args[0] == "update" {
			cur := atomic.AddInt32(&concurrent, 1)
			defer atomic.AddInt32(&concurrent, -1)
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
					break
				}
			}
			// Long enough that a race would reliably overlap the two
			// goroutines inside the critical section if the lock were
			// missing.
			time.Sleep(20 * time.Millisecond)
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { runAs = previousRunAs })

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, name := range []string{"amp1", "amp2"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := updateAmp(name); err != nil {
				t.Errorf("updateAmp(%s): %v", name, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if maxConcurrent != 1 {
		t.Fatalf("max concurrent `amp update` invocations across instances sharing HOME = %d, want 1 (not serialized via withNpmGlobalLock)", maxConcurrent)
	}
}

// TestUpdateAmpStillRunsAmpUpdatePorcelain confirms the lock wraps the
// existing command rather than replacing it with something else.
func TestUpdateAmpStillRunsAmpUpdatePorcelain(t *testing.T) {
	envDir := withEnvDir(t)
	withFakeUserLookup(t)

	if err := os.WriteFile(filepath.Join(envDir, "probe.env"), []byte("AGENTMUX_INSTANCE_NAME=probe\nAGENTMUX_AGENT=amp\nAGENTMUX_RUN_USER=someuser\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotName string
	var gotArgs []string
	previousRunAs := runAs
	runAs = func(runUser, name string, args ...string) *exec.Cmd {
		if name == "amp" && len(args) > 0 && args[0] == "update" {
			gotName = name
			gotArgs = append([]string{}, args...)
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { runAs = previousRunAs })

	if err := updateAmp("probe"); err != nil {
		t.Fatalf("updateAmp: %v", err)
	}
	if gotName != "amp" || strings.Join(gotArgs, " ") != "update --porcelain" {
		t.Errorf("updateAmp ran %s %v, want amp update --porcelain", gotName, gotArgs)
	}
}
