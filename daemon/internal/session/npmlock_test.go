package session

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Reproduces the actual bug: N callers sharing one home directory, all
// starting at once (matching every opencode instance's identical nightly
// StartCalendarInterval), must be serialized rather than allowed to run
// their npm install concurrently.
func TestWithNpmGlobalLockSerializes(t *testing.T) {
	home := t.TempDir()

	const n = 6
	var (
		concurrent    int32
		maxConcurrent int32
		wg            sync.WaitGroup
		start         = make(chan struct{})
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := withNpmGlobalLock(home, nil, func() error {
				cur := atomic.AddInt32(&concurrent, 1)
				defer atomic.AddInt32(&concurrent, -1)
				for {
					old := atomic.LoadInt32(&maxConcurrent)
					if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
						break
					}
				}
				// Long enough that a race would reliably overlap two
				// goroutines inside the critical section.
				time.Sleep(20 * time.Millisecond)
				return nil
			})
			if err != nil {
				t.Errorf("withNpmGlobalLock: %v", err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if maxConcurrent != 1 {
		t.Fatalf("max concurrent holders of the lock = %d, want 1 (lock did not serialize)", maxConcurrent)
	}
}

func TestWithNpmGlobalLockCreatesLockDirWhenMissing(t *testing.T) {
	home := t.TempDir()

	if err := withNpmGlobalLock(home, nil, func() error { return nil }); err != nil {
		t.Fatalf("withNpmGlobalLock: %v", err)
	}

	if _, err := os.Stat(home + "/.agentmux/npm-update.lock"); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}
