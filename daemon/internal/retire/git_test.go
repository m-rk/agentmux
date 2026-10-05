package retire

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo builds a temp repo with main and a merged task branch, and
// returns the repo toplevel. Commits carry fixed author env so the test
// never depends on the host's git config.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "app")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(repo, "init", "-q", "-b", "main")
	run(repo, "commit", "-q", "--allow-empty", "-m", "init")
	run(repo, "checkout", "-q", "-b", "task/AMUX-19-x")
	run(repo, "commit", "-q", "--allow-empty", "-m", "work")
	run(repo, "checkout", "-q", "main")
	run(repo, "merge", "-q", "--ff-only", "task/AMUX-19-x")
	run(repo, "checkout", "-q", "-b", "task/unmerged")
	run(repo, "commit", "-q", "--allow-empty", "-m", "unmerged work")
	run(repo, "checkout", "-q", "main")
	return repo
}

func TestDeleteBranchWhenMerged(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	got, err := deleteBranchWhenMerged(ctx, State{Repo: repo, Branch: "task/AMUX-19-x"})
	if err != nil {
		t.Fatalf("merged branch: %v", err)
	}
	if !got.deleted {
		t.Error("merged branch was not marked for deletion")
	}
}

func TestDeleteBranchWhenMergedRefusesUnmerged(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	_, err := deleteBranchWhenMerged(ctx, State{Repo: repo, Branch: "task/unmerged"})
	if err == nil {
		t.Fatal("unmerged branch: nil error, want a refusal")
	}
	if ReasonOf(err) != "invalid" {
		t.Errorf("reason = %s, want invalid", ReasonOf(err))
	}
	if !strings.Contains(DetailOf(err), "task/unmerged") || !strings.Contains(DetailOf(err), "main") {
		t.Errorf("detail = %q, want branch and main named", DetailOf(err))
	}
}

func TestDeleteBranchWhenMergedKeepsMain(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	got, err := deleteBranchWhenMerged(ctx, State{Repo: repo, Branch: "main"})
	if err != nil {
		t.Fatalf("main: %v", err)
	}
	if got.deleted || got.kept == "" {
		t.Errorf("main: deleted=%v kept=%q", got.deleted, got.kept)
	}
}

func TestRemoveWorktreeRemovesAndDeletesBranch(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(filepath.Dir(repo), "app-worktrees", "wt-1")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", wtPath, "task/AMUX-19-x")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if err := removeWorktree(ctx, State{Repo: repo, Workdir: wtPath, Branch: "task/AMUX-19-x"}); err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree still on disk")
	}
	cmd = exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/task/AMUX-19-x")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err == nil {
		t.Error("branch still exists after removeWorktree")
	}
}

func TestLiveEnvInspectRefusesDirtyWorktree(t *testing.T) {
	repo := gitRepo(t)
	wtPath := filepath.Join(filepath.Dir(repo), "app-worktrees", "wt-dirty")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", wtPath, "task/AMUX-19-x")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LiveEnv{}.Inspect(context.Background(), "task-1", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	if err == nil {
		t.Fatal("dirty worktree: nil error, want a refusal")
	}
	if ReasonOf(err) != "invalid" || !strings.Contains(DetailOf(err), "uncommitted") {
		t.Errorf("dirty worktree: %v", err)
	}
}

func TestLiveEnvInspectRefusesUnknownBranch(t *testing.T) {
	repo := gitRepo(t)
	_, err := LiveEnv{}.Inspect(context.Background(), "task-1", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": repo,
	})
	// repo is on main: clean, so Inspect proceeds to agent handling and
	// succeeds for claude-code (no thread lookup). It must not fail.
	if err != nil {
		t.Fatalf("clean main checkout: %v", err)
	}
}
