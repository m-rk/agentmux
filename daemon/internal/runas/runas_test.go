package runas

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentUserCommandContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CurrentUserCommandContext(ctx, "sh", "-c", "exit 0").Run(); err == nil {
		t.Fatal("cancelled command unexpectedly ran")
	}
}

func TestCommandFailsClosedWhenRunUserIsMissing(t *testing.T) {
	cmd := Command("agentmux-user-that-cannot-exist-7f64d8", "sh", "-c", "exit 0")
	if cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "looking up run user") {
		t.Fatalf("cmd.Err = %v", cmd.Err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("missing run user unexpectedly executed the command")
	}
}

func TestCommandForCurrentUserPreservesExistingCredentials(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cmd := Command(u.Username, "sh", "-c", "exit 0")
	if cmd.Err != nil {
		t.Fatalf("Command: %v", cmd.Err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		t.Fatal("same-user command should preserve the process's credentials and supplementary groups")
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("same-user command failed: %v", err)
	}
}

func TestCredentialForRejectsInvalidIDs(t *testing.T) {
	if _, err := credentialFor(&user.User{Username: "broken", Uid: "not-a-uid", Gid: "1"}); err == nil {
		t.Fatal("invalid UID unexpectedly accepted")
	}
	if _, err := credentialFor(&user.User{Username: "broken", Uid: "1", Gid: "not-a-gid"}); err == nil {
		t.Fatal("invalid GID unexpectedly accepted")
	}
}

func TestCommandRejectsBinaryOutsideRunUserPath(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cmd := Command(u.Username, "agentmux-command-that-cannot-exist-3d18b7")
	if cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "not found") {
		t.Fatalf("cmd.Err = %v", cmd.Err)
	}
}

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
