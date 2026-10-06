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

// fakeStore is the test Store: in-memory swept records.
type fakeStore struct {
	recs    map[string]SweptRecord
	loadErr error
}

func (f *fakeStore) Save(rec SweptRecord) error {
	if f.recs == nil {
		f.recs = map[string]SweptRecord{}
	}
	f.recs[rec.Thread] = rec
	return nil
}

func (f *fakeStore) Load() ([]SweptRecord, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	var out []SweptRecord
	for _, r := range f.recs {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeStore) Remove(thread string) error {
	delete(f.recs, thread)
	return nil
}

var gcNow = time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)

func TestGCDeletesOnlyDue(t *testing.T) {
	due := "T-00000000-0000-4000-8000-000000000031"
	fresh := "T-00000000-0000-4000-8000-000000000032"
	store := &fakeStore{recs: map[string]SweptRecord{
		due:   {Thread: due, ArchivedAt: gcNow.Add(-15 * 24 * time.Hour)},
		fresh: {Thread: fresh, ArchivedAt: gcNow.Add(-3 * 24 * time.Hour)},
	}}
	runner := &fakeRunner{
		exports: map[string]*ExportedThread{
			due: exported(due, "relay", "[relayed by x]", "idle", 15*24*time.Hour, 1),
		},
		exportErr: map[string]error{fresh: context.DeadlineExceeded},
	}
	// A missing retention file means the 14-day default.
	res, err := GC(context.Background(), store, runner, filepath.Join(t.TempDir(), "retention.yaml"), false, gcNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.RetentionDays != 14 {
		t.Errorf("RetentionDays = %d, want 14", res.RetentionDays)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != due {
		t.Errorf("Deleted = %v, want [%s]", res.Deleted, due)
	}
	if len(res.Kept) != 1 || res.Kept[0].Thread != fresh {
		t.Errorf("Kept = %v, want [%s]", res.Kept, fresh)
	}
	if len(runner.deleted) != 1 || runner.deleted[0] != due {
		t.Errorf("deleted = %v", runner.deleted)
	}
	if _, ok := store.recs[due]; ok {
		t.Error("due record was not removed")
	}
	if _, ok := store.recs[fresh]; !ok {
		t.Error("fresh record was removed")
	}
}

func TestGCDropsGoneThreads(t *testing.T) {
	gone := "T-00000000-0000-4000-8000-000000000033"
	store := &fakeStore{recs: map[string]SweptRecord{
		gone: {Thread: gone, ArchivedAt: gcNow.Add(-30 * 24 * time.Hour)},
	}}
	// No exports at all: the thread is already gone from the account.
	runner := &fakeRunner{exports: map[string]*ExportedThread{}}
	res, err := GC(context.Background(), store, runner, filepath.Join(t.TempDir(), "retention.yaml"), false, gcNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 || res.Missing[0] != gone {
		t.Errorf("Missing = %v, want [%s]", res.Missing, gone)
	}
	if len(runner.deleted) != 0 {
		t.Errorf("deleted = %v: a gone thread needs no delete", runner.deleted)
	}
	if len(store.recs) != 0 {
		t.Errorf("records = %v: gone threads must not linger", store.recs)
	}
}

func TestGCDryRunDeletesNothing(t *testing.T) {
	due := "T-00000000-0000-4000-8000-000000000034"
	store := &fakeStore{recs: map[string]SweptRecord{
		due: {Thread: due, ArchivedAt: gcNow.Add(-30 * 24 * time.Hour)},
	}}
	runner := &fakeRunner{exports: map[string]*ExportedThread{
		due: exported(due, "r", "x", "idle", 30*24*time.Hour, 1),
	}}
	res, err := GC(context.Background(), store, runner, filepath.Join(t.TempDir(), "retention.yaml"), true, gcNow)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Deleted) != 1 {
		t.Fatalf("dry run = %+v", res)
	}
	if len(runner.deleted) != 0 || len(store.recs) != 1 {
		t.Fatal("dry run changed something")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	store := FileStore{Home: home}
	rec := SweptRecord{Thread: "T-00000000-0000-4000-8000-000000000035", Reason: "relay stray", ArchivedAt: gcNow}
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
