package ops

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureCodexDefaultsUsesMergenticConfig(t *testing.T) {
	home := t.TempDir()
	localState := filepath.Join(home, "state")
	vault := filepath.Join(home, "vault")
	if err := os.MkdirAll(localState, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localState, "config.json"), []byte(`{"vault_path":"`+vault+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MERGENTIC_LOCAL_STATE", localState)
	t.Setenv("MERGENTIC_VAULT", "")
	want := []string{localState, filepath.Join(vault, "projects")}
	got, err := EnsureCodexDefaults(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dirs = %#v, want %#v", got, want)
	}
	got = readCodexAddDirs(filepath.Join(home, ".config", "agentmux", "codex.yaml"))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved dirs = %#v, want %#v", got, want)
	}
}

func TestEnsureCodexDefaultsPreservesExistingFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "agentmux", "codex.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("add_dirs:\n  - /custom/write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureCodexDefaults(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"/custom/write"}) {
		t.Fatalf("dirs = %#v", got)
	}
}
