package retention

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingMeansDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "no-such-file.yaml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg.RetentionDays != DefaultDays {
		t.Errorf("RetentionDays = %d, want default %d", cfg.RetentionDays, DefaultDays)
	}
}

func TestLoadParsesDays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retention.yaml")
	if err := os.WriteFile(path, []byte("retention_days: 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", cfg.RetentionDays)
	}
}

func TestLoadRefusesNonPositive(t *testing.T) {
	for _, body := range []string{"retention_days: 0\n", "retention_days: -3\n", "{}\n"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "retention.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("Load(%q) = nil, want an error", body)
		}
	}
}

func TestLoadRefusesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retention.yaml")
	if err := os.WriteFile(path, []byte(":\tbad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load(garbage) = nil, want an error")
	}
}
