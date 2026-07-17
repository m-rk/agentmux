package runas

import (
	"os"
	"path/filepath"
	"testing"
)

// makeExecutable creates dir/name as an executable file, returning its
// full path.
func makeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLookPathIn(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	execPath := makeExecutable(t, dirB, "mytool")
	// A non-executable file with the same kind of name, in the dir searched
	// first — lookPathIn must skip it and keep looking rather than stopping
	// here just because a file with the right name exists.
	if err := os.WriteFile(filepath.Join(dirA, "not-executable"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory literally named like the target — must not be returned
	// (os.Stat succeeds on it, but it's not a file lookPathIn should exec).
	if err := os.Mkdir(filepath.Join(dirA, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("finds an executable further down the path", func(t *testing.T) {
		path := dirA + ":" + dirB
		if got := lookPathIn("mytool", path); got != execPath {
			t.Errorf("lookPathIn = %q, want %q", got, execPath)
		}
	})

	t.Run("skips a non-executable file with the target name", func(t *testing.T) {
		if got := lookPathIn("not-executable", dirA); got != "" {
			t.Errorf("lookPathIn = %q, want \"\" (file exists but isn't executable)", got)
		}
	})

	t.Run("skips a directory with the target name", func(t *testing.T) {
		if got := lookPathIn("adir", dirA); got != "" {
			t.Errorf("lookPathIn = %q, want \"\" (a directory, not an executable file)", got)
		}
	})

	t.Run("not found anywhere returns empty", func(t *testing.T) {
		if got := lookPathIn("does-not-exist-anywhere", dirA+":"+dirB); got != "" {
			t.Errorf("lookPathIn = %q, want \"\"", got)
		}
	})

	t.Run("tolerates empty path segments", func(t *testing.T) {
		path := ":" + dirA + "::" + dirB + ":"
		if got := lookPathIn("mytool", path); got != execPath {
			t.Errorf("lookPathIn = %q, want %q", got, execPath)
		}
	})
}

func TestCurrentUserLookPath(t *testing.T) {
	dir := t.TempDir()
	execPath := makeExecutable(t, dir, "probetool")

	prevPath := os.Getenv("PATH")
	os.Setenv("PATH", dir)
	t.Cleanup(func() { os.Setenv("PATH", prevPath) })

	got, err := CurrentUserLookPath("probetool")
	if err != nil {
		t.Fatalf("CurrentUserLookPath: %v", err)
	}
	if got != execPath {
		t.Errorf("CurrentUserLookPath = %q, want %q", got, execPath)
	}

	if _, err := CurrentUserLookPath("does-not-exist-anywhere"); err == nil {
		t.Error("CurrentUserLookPath(does-not-exist-anywhere) = nil error, want an error")
	}
}

func TestCurrentUserCommandResolvesAgainstFixedUpPath(t *testing.T) {
	// This is the exact bug class internal/runas exists to prevent: a
	// caller whose own ambient $PATH is minimal (as under launchd/systemd)
	// must still be able to resolve a tool living in one of the extra
	// directories CurrentUserCommand always searches.
	dir := t.TempDir()

	prevHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	prevPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent-minimal-path")
	t.Cleanup(func() {
		os.Setenv("HOME", prevHome)
		os.Setenv("PATH", prevPath)
	})

	// probetool lives directly in $HOME/.local/bin, one of the fixed
	// per-user directories CurrentUserCommand always prepends.
	localBin := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	execPath := makeExecutable(t, localBin, "probetool")

	cmd := CurrentUserCommand("probetool")
	if cmd.Path != execPath {
		t.Errorf("cmd.Path = %q, want %q (should resolve via $HOME/.local/bin even though ambient $PATH doesn't have it)", cmd.Path, execPath)
	}
}
