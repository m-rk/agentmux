package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveReposRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := mustReposRoot(t, ""), filepath.Join(home, "Projects"); got != want {
		t.Fatalf("default root = %q, want %q", got, want)
	}
	if got, want := mustReposRoot(t, "~/src"), filepath.Join(home, "src"); got != want {
		t.Fatalf("expanded root = %q, want %q", got, want)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "agentmux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "agentmux", "repos.yaml"), []byte("repos_root: /srv/repos\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustReposRoot(t, ""); got != "/srv/repos" {
		t.Fatalf("configured root = %q", got)
	}
	if got := mustReposRoot(t, "/tmp/override"); got != "/tmp/override" {
		t.Fatalf("override root = %q", got)
	}
}

func mustReposRoot(t *testing.T, override string) string {
	t.Helper()
	got, err := resolveReposRoot(override)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestResolveReposRootRequiresAbsolute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := resolveReposRoot("relative"); err == nil {
		t.Fatal("expected relative root to be refused")
	}
}
