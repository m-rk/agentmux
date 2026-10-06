package ampsweep

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// fakeInstances is the test Instances: canned live instances.
type fakeInstances struct {
	live []LiveInstance
	err  error
}

func (f *fakeInstances) Live(context.Context) ([]LiveInstance, error) {
	return f.live, f.err
}

func threadsOf(ids ...string) func(context.Context, transcript.Source) ([]transcript.Thread, error) {
	return func(context.Context, transcript.Source) ([]transcript.Thread, error) {
		var out []transcript.Thread
		for _, id := range ids {
			out = append(out, transcript.Thread{ID: id})
		}
		return out, nil
	}
}

// writeAmpRunLog records a fake `sessions run` thread log for instance.
func writeAmpRunLog(t *testing.T, home, instance, thread string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "state", "agentmux", "sessions", instance)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "amp-run-"+thread+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLiveThreadsFindsRecordedAndListed(t *testing.T) {
	home := t.TempDir()
	rec := "T-00000000-0000-4000-8000-000000000021"
	writeAmpRunLog(t, home, "task-1", rec)
	listedID := "T-00000000-0000-4000-8000-000000000022"
	inst := &fakeInstances{live: []LiveInstance{{Name: "task-1", Runner: "r", Home: home}}}
	live := LiveThreads(context.Background(), inst, threadsOf(listedID))
	if !live[rec] || !live[listedID] {
		t.Fatalf("live = %v, want both %s and %s", live, rec, listedID)
	}
}

func TestLiveThreadsSkipsNonTask(t *testing.T) {
	home := t.TempDir()
	rec := "T-00000000-0000-4000-8000-000000000023"
	writeAmpRunLog(t, home, "mergentic-amp", rec)
	inst := &fakeInstances{live: []LiveInstance{{Name: "mergentic-amp", Runner: "r", Home: home}}}
	live := LiveThreads(context.Background(), inst, threadsOf(rec))
	if live[rec] {
		t.Fatalf("live = %v: long-lived agents are not dispatched workers", live)
	}
}

func TestRecordedThreadsSkipsPending(t *testing.T) {
	home := t.TempDir()
	writeAmpRunLog(t, home, "task-1", "pending")
	if got := recordedThreads(home, "task-1"); len(got) != 0 {
		t.Fatalf("recorded = %v, want none (pending names no thread)", got)
	}
}

func TestRecordedThreadsMissingDir(t *testing.T) {
	if got := recordedThreads(t.TempDir(), "task-1"); len(got) != 0 {
		t.Fatalf("recorded = %v, want nil", got)
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	store := FileStore{Home: home}
	now := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	rec := SweptRecord{Thread: "T-00000000-0000-4000-8000-000000000035", Reason: "relay stray", ArchivedAt: now}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded) != 1 || loaded[0].Thread != rec.Thread {
		t.Fatalf("loaded = %+v, err = %v", loaded, err)
	}
	if err := store.Remove(rec.Thread); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Load()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("after remove: %+v, err = %v", loaded, err)
	}
}
