package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeCodex(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestCodexSandboxProblem(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := codexSandboxProblem(context.Background()); got != "" {
		t.Fatalf("no codex installed: got %q", got)
	}
	fakeCodex(t, "exit 0")
	if got := codexSandboxProblem(context.Background()); got != "" {
		t.Fatalf("working sandbox: got %q", got)
	}
	fakeCodex(t, "echo 'bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted' >&2; exit 1")
	got := codexSandboxProblem(context.Background())
	if !strings.Contains(got, "RTM_NEWADDR") || !strings.Contains(got, "docs/codex-sandbox.md") {
		t.Fatalf("failing sandbox: got %q", got)
	}
}
