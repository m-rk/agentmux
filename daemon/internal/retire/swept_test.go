package retire

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// saveSweptRecord writes one swept-thread record under home for tests.
func saveSweptRecord(t *testing.T, home string, rec sweptRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, sweptDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, sweptDirName, rec.Thread+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGCDeletesDueSweptThreads(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	due := "T-00000000-0000-4000-8000-000000000041"
	fresh := "T-00000000-0000-4000-8000-000000000042"
	gone := "T-00000000-0000-4000-8000-000000000043"
	saveSweptRecord(t, env.home, sweptRecord{Thread: due, Reason: "relay stray", ArchivedAt: now.Add(-15 * 24 * time.Hour)})
	saveSweptRecord(t, env.home, sweptRecord{Thread: fresh, Reason: "short idle thread", ArchivedAt: now.Add(-3 * 24 * time.Hour)})
	saveSweptRecord(t, env.home, sweptRecord{Thread: gone, Reason: "relay stray", ArchivedAt: now.Add(-30 * 24 * time.Hour)})
	env.sweptGone = map[string]bool{gone: true}

	res, err := GC(context.Background(), env, false, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.SweptDeleted) != 1 || res.SweptDeleted[0] != due {
		t.Errorf("SweptDeleted = %v, want [%s]", res.SweptDeleted, due)
	}
	if len(res.SweptMissing) != 1 || res.SweptMissing[0] != gone {
		t.Errorf("SweptMissing = %v, want [%s]", res.SweptMissing, gone)
	}
	if len(res.SweptKept) != 1 || res.SweptKept[0].Instance != fresh {
		t.Errorf("SweptKept = %v, want [%s]", res.SweptKept, fresh)
	}
	if res.SweptKept[0].DeleteAt == "" {
		t.Error("swept kept entry has no delete_at")
	}
	if len(env.sweptDeleted) != 1 || env.sweptDeleted[0] != due {
		t.Errorf("sweep deletes = %v (the gone thread needs no delete)", env.sweptDeleted)
	}
	if _, err := os.Stat(filepath.Join(env.home, sweptDirName, due+".json")); !os.IsNotExist(err) {
		t.Error("due swept record was not removed")
	}
	if _, err := os.Stat(filepath.Join(env.home, sweptDirName, gone+".json")); !os.IsNotExist(err) {
		t.Error("gone swept record was not removed")
	}
	if _, err := os.Stat(filepath.Join(env.home, sweptDirName, fresh+".json")); err != nil {
		t.Error("fresh swept record was removed")
	}
}

func TestGCSweptDryRunDeletesNothing(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	due := "T-00000000-0000-4000-8000-000000000044"
	saveSweptRecord(t, env.home, sweptRecord{Thread: due, ArchivedAt: now.Add(-30 * 24 * time.Hour)})
	res, err := GC(context.Background(), env, true, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if !res.DryRun || len(res.SweptDeleted) != 1 || res.SweptDeleted[0] != due {
		t.Fatalf("dry run = %+v", res)
	}
	if len(env.sweptDeleted) != 0 {
		t.Fatalf("dry run deleted: %v", env.sweptDeleted)
	}
	if _, err := os.Stat(filepath.Join(env.home, sweptDirName, due+".json")); err != nil {
		t.Error("dry run removed the record")
	}
}

// TestGCSweptAndRetiredShareRetention: both halves of one gc age out
// together — a swept thread archived the same day as a retire is due
// the same day.
func TestGCSweptAndRetiredShareRetention(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	archived := now.Add(-15 * 24 * time.Hour)
	saveTestRecord(t, env.home, Record{Instance: "task-old", Agent: "amp",
		RetiredAt:  archived,
		AmpThreads: []string{"T-00000000-0000-4000-8000-000000000045"}})
	thread := "T-00000000-0000-4000-8000-000000000046"
	saveSweptRecord(t, env.home, sweptRecord{Thread: thread, ArchivedAt: archived})
	res, err := GC(context.Background(), env, false, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.Deleted) != 1 || len(res.SweptDeleted) != 1 {
		t.Errorf("retired deleted = %v, swept deleted = %v: same age must mean same verdict", res.Deleted, res.SweptDeleted)
	}
}

// TestGCIgnoresCorruptSweptRecord: a corrupt swept file never blocks
// the gc from collecting the rest.
func TestGCIgnoresCorruptSweptRecord(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	if err := os.MkdirAll(filepath.Join(env.home, sweptDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.home, sweptDirName, "broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	due := "T-00000000-0000-4000-8000-000000000047"
	saveSweptRecord(t, env.home, sweptRecord{Thread: due, ArchivedAt: now.Add(-30 * 24 * time.Hour)})
	res, err := GC(context.Background(), env, false, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.SweptDeleted) != 1 || res.SweptDeleted[0] != due {
		t.Errorf("SweptDeleted = %v, want [%s]", res.SweptDeleted, due)
	}
}
