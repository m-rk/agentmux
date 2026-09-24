package threadwatch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOffsetsMissingDir(t *testing.T) {
	dir := t.TempDir()
	fo, err := LoadOffsets(filepath.Join(dir, "does-not-exist-yet"))
	if err != nil {
		t.Fatalf("LoadOffsets: %v", err)
	}
	if _, ok := fo.Get("whatever"); ok {
		t.Error("expected empty offsets from a missing file")
	}
}

func TestFileOffsetsGetSetSave(t *testing.T) {
	dir := t.TempDir()
	fo, err := LoadOffsets(dir)
	if err != nil {
		t.Fatalf("LoadOffsets: %v", err)
	}

	if _, ok := fo.Get("claude:/foo"); ok {
		t.Fatal("expected no offset before Set")
	}
	fo.Set("claude:/foo", 1234)
	fo.Set("amp:/bar", 42)

	if got, ok := fo.Get("claude:/foo"); !ok || got != 1234 {
		t.Fatalf("Get after Set = (%v, %v), want (1234, true)", got, ok)
	}

	if err := fo.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(dir, "offsets.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat offsets.json: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("offsets.json mode = %v, want 0600", info.Mode().Perm())
	}

	reloaded, err := LoadOffsets(dir)
	if err != nil {
		t.Fatalf("LoadOffsets (reload): %v", err)
	}
	if got, ok := reloaded.Get("claude:/foo"); !ok || got != 1234 {
		t.Fatalf("reloaded Get(claude:/foo) = (%v, %v), want (1234, true)", got, ok)
	}
	if got, ok := reloaded.Get("amp:/bar"); !ok || got != 42 {
		t.Fatalf("reloaded Get(amp:/bar) = (%v, %v), want (42, true)", got, ok)
	}
}

func TestFileOffsetsSaveCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	fo, err := LoadOffsets(dir)
	if err != nil {
		t.Fatalf("LoadOffsets: %v", err)
	}
	fo.Set("k", 7)
	if err := fo.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "offsets.json")); err != nil {
		t.Fatalf("expected offsets.json to be created: %v", err)
	}
}

func TestFileOffsetsConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	fo, err := LoadOffsets(dir)
	if err != nil {
		t.Fatalf("LoadOffsets: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fo.Set("key", int64(i))
			fo.Get("key")
		}(i)
	}
	wg.Wait()

	if err := fo.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}
