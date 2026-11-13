package retire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/preview"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// TestApplyCodexStopsRunningChildAndKeepsBranch retires a codex task
// instance whose `codex exec` child is still hanging: the child must be
// killed before the worktree goes, and the unmerged branch must survive.
// The codex is the synthetic fake; nothing live runs.
func TestApplyCodexStopsRunningChildAndKeepsBranch(t *testing.T) {
	ctx := context.Background()
	repo := originRepo(t)
	work := filepath.Join(filepath.Dir(repo), "task-cx")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "task/cx-work", work)
	gitRun(t, work, "commit", "-q", "--allow-empty", "-m", "unmerged work")

	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fakecodex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexec "+fake+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("FAKE_CODEX_SCENARIO", "hang")
	t.Setenv("FAKE_CODEX_HANG_SECONDS", "60")
	t.Setenv("FAKE_CODEX_THREAD_ID", "00000000-0000-4000-8000-0000000000c3")

	logPath := session.CodexRunLogPath(home, "task-cx", "")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = session.StartCodexRun(ctx, "task-cx", []string{"exec", "--json", "-"}, "x", work, logPath)
	if err != nil {
		t.Fatalf("starting the fake run: %v", err)
	}
	t.Cleanup(func() { session.StopCodexRuns("task-cx", "") })
	if st := session.CodexRunStateOf(logPath); st.State != "running" {
		t.Fatalf("before retire the run is %+v, want running", st)
	}

	st := State{Workdir: work, Repo: repo, Branch: "task/cx-work",
		Branches: []BranchFate{{Branch: "task/cx-work", Kept: "not merged"}}}
	removed := false
	cache := session.CodexGoCacheDir(home, "task-cx")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := apply(ctx, func(context.Context, string) (string, error) { removed = true; return "", nil },
		"task-cx", "codex", map[string]string{session.CodexGoCacheKey: cache}, st)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !removed {
		t.Error("managed half was not removed")
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("go cache %s survived retire (%v)", cache, err)
	}
	if res.BranchDeleted || res.BranchKept == "" {
		t.Errorf("branch fate = %+v, want kept", res)
	}
	if out := gitRun(t, repo, "branch", "--list", "task/cx-work"); !strings.Contains(out, "task/cx-work") {
		t.Errorf("branch task/cx-work is gone: %q", out)
	}
	if _, err := os.Stat(work); err == nil {
		t.Error("worktree still present")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := session.CodexRunStateOf(logPath); st.State == "failed" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run child was not stopped: %+v", session.CodexRunStateOf(logPath))
}

func TestRemovePreviewStoreDropsOnlyTheInstancesCaptures(t *testing.T) {
	home := t.TempDir()
	mine := preview.StoreDir(home, "task-a")
	other := preview.StoreDir(home, "task-b")
	for _, d := range []string{mine, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "capture-1.png"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removePreviewStore(home, "task-a")
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("captures for the retired instance survived (%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another instance's captures were removed: %v", err)
	}
	// Names that are not a plain task instance, and a relative home, remove nothing.
	removePreviewStore(home, "task-b/../task-b")
	removePreviewStore(home, "forgentic")
	removePreviewStore("relative", "task-b")
	if _, err := os.Stat(other); err != nil {
		t.Errorf("refused names still removed captures: %v", err)
	}
}
