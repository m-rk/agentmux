package retire

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs git in dir and returns trimmed combined output. Commits
// carry fixed author env so tests never depend on the host's git config.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
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

// gitRepo builds a temp repo with main and a merged task branch, and
// returns the repo toplevel.
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
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, repo, "checkout", "-q", "-b", "task/AMUX-19-x")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--ff-only", "task/AMUX-19-x")
	gitRun(t, repo, "checkout", "-q", "-b", "task/unmerged")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "unmerged work")
	gitRun(t, repo, "checkout", "-q", "main")
	return repo
}

// originRepo builds a repo pair: origin (bare) plus a clone whose
// origin/HEAD points at origin's main, so fetch-based checks run fully
// offline against file://. Returns the clone path.
func originRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	gitRun(t, root, "init", "-q", "--bare", "-b", "main", "origin.git")
	gitRun(t, root, "clone", "-q", origin, "clone")
	gitRun(t, clone, "config", "user.name", "t")
	gitRun(t, clone, "config", "user.email", "t@example.com")
	gitRun(t, clone, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, clone, "push", "-q", "origin", "main")
	return clone
}

func TestVerifyAndDeleteMergedBranch(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	git := gitRunner("")
	gitRun(t, repo, "checkout", "-q", "-b", "task/merged")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--ff-only", "task/merged")
	gitRun(t, repo, "push", "-q", "origin", "main")
	st := State{Repo: repo, Branch: "task/merged",
		Branches: []BranchFate{{Branch: "task/merged", Deleted: true}}}
	verified, err := verifyBranches(ctx, git, st)
	if err != nil {
		t.Fatalf("verifyBranches: %v", err)
	}
	if len(verified) != 1 {
		t.Fatalf("verified = %v, want the merged branch", verified)
	}
	fates, err := deleteVerifiedBranches(ctx, git, st, mainOf(t, ctx, st), verified)
	if err != nil {
		t.Fatalf("deleteVerifiedBranches: %v", err)
	}
	if len(fates) != 1 || !fates[0].Deleted {
		t.Errorf("fates = %+v, want one deletion", fates)
	}
}

// TestUnverifiedBranchIsKept is the AMUX-20 regression: a branch whose
// safety was never verified must be kept with its reason, never deleted
// on the strength of local refs alone — and the retire completes.
func TestUnverifiedBranchIsKept(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	git := gitRunner("")
	gitRun(t, repo, "checkout", "-q", "-b", "task/never-checked")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, repo, "checkout", "-q", "main")
	st := State{Repo: repo, Branch: "task/never-checked",
		Branches: []BranchFate{{Branch: "task/never-checked", Kept: "not verified"}}}
	verified, err := verifyBranches(ctx, git, st)
	if err != nil {
		t.Fatalf("verifyBranches: %v", err)
	}
	if len(verified) != 0 {
		t.Fatalf("verified = %v, want none", verified)
	}
	fates, err := deleteVerifiedBranches(ctx, git, st, mainOf(t, ctx, st), verified)
	if err != nil {
		t.Fatalf("deleteVerifiedBranches: %v", err)
	}
	if len(fates) != 1 || fates[0].Deleted || fates[0].Kept == "" {
		t.Errorf("fates = %+v, want one keep with a reason", fates)
	}
}

func TestVerifyKeepsUnmergedBranch(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	git := gitRunner("")
	st := State{Repo: repo, Branch: "task/unmerged",
		Branches: []BranchFate{{Branch: "task/unmerged",
			Kept: "branch task/unmerged has commits not on origin/main; merge it before retiring"}}}
	verified, err := verifyBranches(ctx, git, st)
	if err != nil {
		t.Fatalf("verifyBranches: %v", err)
	}
	if len(verified) != 0 {
		t.Errorf("verified = %v, want none for an unmerged branch", verified)
	}
	fates, err := deleteVerifiedBranches(ctx, git, st, mainOf(t, ctx, st), verified)
	if err != nil {
		t.Fatalf("deleteVerifiedBranches: %v", err)
	}
	if len(fates) != 1 || fates[0].Deleted {
		t.Errorf("fates = %+v, want the branch kept", fates)
	}
	if !strings.Contains(fates[0].Kept, "task/unmerged") || !strings.Contains(fates[0].Kept, "origin/main") {
		t.Errorf("kept = %q, want branch and upstream named", fates[0].Kept)
	}
}

func TestVerifyKeepsMain(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	git := gitRunner("")
	st := State{Repo: repo, Branches: []BranchFate{{Branch: "main", Kept: "not a task branch"}}}
	verified, err := verifyBranches(ctx, git, st)
	if err != nil {
		t.Fatalf("verifyBranches: %v", err)
	}
	if len(verified) != 0 {
		t.Errorf("verified = %v, want none for main", verified)
	}
	fates, err := deleteVerifiedBranches(ctx, git, st, mainOf(t, ctx, st), verified)
	if err != nil {
		t.Fatalf("deleteVerifiedBranches: %v", err)
	}
	if len(fates) != 1 || fates[0].Deleted || fates[0].Kept == "" {
		t.Errorf("fates = %+v, want main kept", fates)
	}
}

// TestCheckBranchSafeRefusesUnpushedWork is the AMUX-20 case: an unpushed
// branch whose tip is nowhere on origin must be kept, even though local
// main is an ancestor of nothing here and `git branch -d` from the
// template checkout would once have said "main contains it" via the
// checkout's own HEAD.
func TestCheckBranchSafeRefusesUnpushedWork(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	gitRun(t, repo, "checkout", "-q", "-b", "task/unpushed")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "unique work")
	ok, why, upstream := checkBranchSafe(ctx, gitRunner(""), repo, "task/unpushed")
	if ok {
		t.Fatal("unpushed branch reported safe, want keep")
	}
	if upstream != "origin/main" {
		t.Errorf("upstream = %q, want origin/main", upstream)
	}
	if !strings.Contains(why, "task/unpushed") {
		t.Errorf("why = %q, want the branch named", why)
	}
}

// TestCheckBranchSafeAcceptsMergedTip covers the normal retire: merged to
// main and pushed, the branch tip is an ancestor of origin/main.
func TestCheckBranchSafeAcceptsMergedTip(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	gitRun(t, repo, "checkout", "-q", "-b", "task/done")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--ff-only", "task/done")
	gitRun(t, repo, "push", "-q", "origin", "main")
	ok, why, upstream := checkBranchSafe(ctx, gitRunner(""), repo, "task/done")
	if !ok {
		t.Fatalf("merged branch not safe: %q", why)
	}
	if upstream != "origin/main" {
		t.Errorf("upstream = %q, want origin/main", upstream)
	}
}

// TestCheckBranchSafeAcceptsSquashMerge covers squash merges: the branch
// tip is not an ancestor of origin/main, but every unique commit has an
// upstream equivalent, so nothing would be lost.
func TestCheckBranchSafeAcceptsSquashMerge(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	gitRun(t, repo, "checkout", "-q", "-b", "task/squashed")
	if err := os.WriteFile(filepath.Join(repo, "feat.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "feat.txt")
	gitRun(t, repo, "commit", "-q", "-m", "add feature")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--squash", "task/squashed")
	gitRun(t, repo, "commit", "-q", "-m", "add feature (squash)")
	gitRun(t, repo, "push", "-q", "origin", "main")
	if err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor",
		"task/squashed", "origin/main").Run(); err == nil {
		t.Fatal("test setup: squash tip unexpectedly an ancestor of origin/main")
	}
	ok, why, _ := checkBranchSafe(ctx, gitRunner(""), repo, "task/squashed")
	if !ok {
		t.Fatalf("squash-merged branch not safe: %q", why)
	}
}

// TestCheckBranchSafeAcceptsCherryPick covers cherry-picks onto main: same
// shape as a squash merge at the cherry level.
func TestCheckBranchSafeAcceptsCherryPick(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	gitRun(t, repo, "checkout", "-q", "-b", "task/picked")
	if err := os.WriteFile(filepath.Join(repo, "pick.txt"), []byte("picked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "pick.txt")
	gitRun(t, repo, "commit", "-q", "-m", "picked change")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "cherry-pick", "task/picked")
	gitRun(t, repo, "push", "-q", "origin", "main")
	ok, why, _ := checkBranchSafe(ctx, gitRunner(""), repo, "task/picked")
	if !ok {
		t.Fatalf("cherry-picked branch not safe: %q", why)
	}
}

// TestCheckBranchSafeRefusesStaleLocalMain is the stale-ref half of
// AMUX-20: the local main contains the branch (e.g. a local merge that
// was never pushed) but origin/main does not. Only origin counts.
func TestCheckBranchSafeRefusesStaleLocalMain(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	gitRun(t, repo, "checkout", "-q", "-b", "task/local-only")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--ff-only", "task/local-only")
	// Deliberately not pushed: origin/main lacks the work.
	ok, why, _ := checkBranchSafe(ctx, gitRunner(""), repo, "task/local-only")
	if ok {
		t.Fatalf("branch only on local main reported safe: %q", why)
	}
}

// TestCheckBranchSafeFetchesBeforeChecking covers a stale origin/main
// tracking ref: someone else pushed the merge after our last fetch.
// The check fetches first, so it still sees the merge.
func TestCheckBranchSafeFetchesBeforeChecking(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	other := filepath.Join(filepath.Dir(repo), "other")
	gitRun(t, filepath.Dir(repo), "clone", "-q", filepath.Join(filepath.Dir(repo), "origin.git"), "other")
	gitRun(t, other, "config", "user.name", "t")
	gitRun(t, other, "config", "user.email", "t@example.com")
	gitRun(t, other, "checkout", "-q", "-b", "task/done-elsewhere")
	gitRun(t, other, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, other, "checkout", "-q", "main")
	gitRun(t, other, "merge", "-q", "--ff-only", "task/done-elsewhere")
	gitRun(t, other, "push", "-q", "origin", "main", "task/done-elsewhere")
	// repo's origin/main is now stale, and the branch exists locally as a
	// worktree branch would; the check must fetch past the stale ref.
	gitRun(t, repo, "fetch", "-q", "origin", "task/done-elsewhere:refs/heads/task/done-elsewhere")
	ok, why, _ := checkBranchSafe(ctx, gitRunner(""), repo, "task/done-elsewhere")
	if !ok {
		t.Fatalf("branch merged upstream not safe without manual fetch: %q", why)
	}
}

func TestRemoveWorktreeRemoves(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(filepath.Dir(repo), "app-worktrees", "wt-1")
	gitRun(t, repo, "worktree", "add", wtPath, "task/AMUX-19-x")
	if err := removeWorktree(ctx, gitRunner(""), State{Repo: repo, Workdir: wtPath, Branch: "task/AMUX-19-x"}); err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree still on disk")
	}
	// Branch deletion is a separate step now (deleteVerifiedBranches):
	// the worktree remove leaves refs alone.
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/task/AMUX-19-x")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		t.Error("worktree remove deleted the branch ref; branch deletion is deleteVerifiedBranches' job")
	}
}

// TestRemoveWorktreeKeepsBranch leaves the ref alone when the branch was
// kept: the worktree goes, the unmerged work stays addressable.
func TestRemoveWorktreeKeepsBranch(t *testing.T) {
	repo := gitRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(filepath.Dir(repo), "app-worktrees", "wt-kept")
	gitRun(t, repo, "worktree", "add", wtPath, "task/unmerged")
	if err := removeWorktree(ctx, gitRunner(""), State{Repo: repo, Workdir: wtPath, Branch: "task/unmerged"}); err != nil {
		t.Fatalf("removeWorktree: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree still on disk")
	}
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/task/unmerged")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		t.Error("kept branch was deleted with its worktree")
	}
}

func TestLiveEnvInspectRefusesDirtyWorktree(t *testing.T) {
	repo := gitRepo(t)
	wtPath := filepath.Join(filepath.Dir(repo), "app-worktrees", "wt-dirty")
	gitRun(t, repo, "worktree", "add", wtPath, "task/AMUX-19-x")
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

// TestLiveEnvInspectMarksUnmergedBranch covers the AMUX-20 dry run: an
// unpushed branch yields a State whose plan keeps the branch with the
// reason, instead of claiming "main contains it".
func TestLiveEnvInspectMarksUnmergedBranch(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(filepath.Dir(repo), "wts", "wt-unpushed")
	gitRun(t, repo, "checkout", "-q", "-b", "task/unpushed")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "unique work")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "worktree", "add", wtPath, "task/unpushed")
	st, err := LiveEnv{}.Inspect(ctx, "task-1", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	kept := fateOf(st, "task/unpushed")
	if kept == nil || kept.Deleted || kept.Kept == "" {
		t.Errorf("Branches = %+v, want task/unpushed kept with a reason", st.Branches)
	}
	joined := strings.Join(st.Plan("claude-code"), "\n")
	if strings.Contains(joined, "delete branch") {
		t.Errorf("plan claims the branch would be deleted: %v", st.Plan("claude-code"))
	}
	if !strings.Contains(joined, "keep branch task/unpushed") {
		t.Errorf("plan does not keep the branch with a reason: %v", st.Plan("claude-code"))
	}
}

// TestLiveEnvInspectMarksMergedBranch is the other half of the dry run:
// a pushed merge yields BranchOK and a delete plan naming the upstream.
func TestLiveEnvInspectMarksMergedBranch(t *testing.T) {
	repo := originRepo(t)
	ctx := context.Background()
	gitRun(t, repo, "checkout", "-q", "-b", "task/done")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "work")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--ff-only", "task/done")
	gitRun(t, repo, "push", "-q", "origin", "main")
	wtPath := filepath.Join(filepath.Dir(repo), "wts", "wt-done")
	gitRun(t, repo, "worktree", "add", wtPath, "task/done")
	st, err := LiveEnv{}.Inspect(ctx, "task-1", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	done := fateOf(st, "task/done")
	if done == nil || !done.Deleted || done.Upstream != "origin/main" {
		t.Errorf("Branches = %+v, want task/done verified for deletion against origin/main", st.Branches)
	}
	if joined := strings.Join(st.Plan("claude-code"), "\n"); !strings.Contains(joined, "delete branch task/done (origin/main contains it)") {
		t.Errorf("plan = %v", st.Plan("claude-code"))
	}
}

// mainOf resolves the main checkout for deleteVerifiedBranches tests.
func mainOf(t *testing.T, ctx context.Context, st State) string {
	t.Helper()
	main, err := mainWorktree(ctx, gitRunner(""), st.Repo)
	if err != nil {
		t.Fatalf("mainWorktree: %v", err)
	}
	return main
}

// fateOf returns the fate for branch, or nil when Inspect found none.
func fateOf(st State, branch string) *BranchFate {
	for i := range st.Branches {
		if st.Branches[i].Branch == branch {
			return &st.Branches[i]
		}
	}
	return nil
}
