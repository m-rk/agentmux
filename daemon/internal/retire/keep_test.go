package retire

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// fakeStopper is the DaemonEnv managed half: records the call, changes
// nothing — the e2e tests below assert the git half against real repos.
type fakeStopper struct {
	calls []string
	err   error
}

func (f *fakeStopper) StopRemove(_ context.Context, instance string) (string, error) {
	f.calls = append(f.calls, instance)
	if f.err != nil {
		return "", f.err
	}
	return "stopped session, removed units and registry entry", nil
}

// e2eRepo builds origin (bare) plus a clone with main pushed, repo-local
// identity so commits never depend on the host's git config, and a
// registry dir with HOME pointed at temp for the retired record.
func e2eRepo(t *testing.T) (root, clone, envDir string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "clone")
	gitRun(t, root, "init", "-q", "--bare", "-b", "main", "origin.git")
	gitRun(t, root, "clone", "-q", origin, "clone")
	gitRun(t, clone, "config", "user.name", "t")
	gitRun(t, clone, "config", "user.email", "t@example.com")
	gitRun(t, clone, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, clone, "push", "-q", "origin", "main")
	envDir = filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	t.Setenv("HOME", filepath.Join(root, "home"))
	return root, clone, envDir
}

func writeRegistry(t *testing.T, envDir, instance string, fields map[string]string) {
	t.Helper()
	var lines []string
	for k, v := range fields {
		lines = append(lines, k+"="+v)
	}
	if err := os.WriteFile(filepath.Join(envDir, instance+".env"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	_, err := gitRunner("")(context.Background(), repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// TestKeepBranchRetire is the AMUX-23 core: a real retire of unmerged
// work succeeds — the agent and worktree go, the branch stays with its
// reason, exactly what the dry run said.
func TestKeepBranchRetire(t *testing.T) {
	_, clone, envDir := e2eRepo(t)
	ctx := context.Background()
	gitRun(t, clone, "checkout", "-q", "-b", "task/AMUX-23-feat")
	gitRun(t, clone, "commit", "-q", "--allow-empty", "-m", "unique work")
	gitRun(t, clone, "checkout", "-q", "main")
	wtPath := filepath.Join(filepath.Dir(clone), "wts", "wt-feat")
	gitRun(t, clone, "worktree", "add", wtPath, "task/AMUX-23-feat")
	writeRegistry(t, envDir, "task-agentmux-amux-23", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	stopper := &fakeStopper{}
	res, err := Retire(ctx, DaemonEnv{Daemon: stopper}, "task-agentmux-amux-23", Options{})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if len(stopper.calls) != 1 {
		t.Errorf("managed half calls = %v, want one", stopper.calls)
	}
	if res.BranchKept == "" || !strings.Contains(res.BranchKept, "task/AMUX-23-feat") {
		t.Errorf("BranchKept = %q, want the kept branch named", res.BranchKept)
	}
	if res.BranchDeleted {
		t.Errorf("BranchDeleted = true, want nothing deleted: %+v", res)
	}
	if len(res.Branches) != 1 || res.Branches[0].Deleted || res.Branches[0].Kept == "" {
		t.Errorf("Branches = %+v, want one keep with a reason", res.Branches)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree still on disk")
	}
	if !branchExists(t, clone, "task/AMUX-23-feat") {
		t.Error("kept branch ref is gone")
	}
	if _, err := os.Stat(recordPath(os.Getenv("HOME"), "task-agentmux-amux-23")); err != nil {
		t.Errorf("retired record missing: %v", err)
	}
}

// TestRetireDeletesMergedKeepsTaskFamily is the AMUX-12 shape: the
// worktree sat on fix/AMUX-12-base-branch (merged upstream) while the
// task branch task/AMUX-12-… lived on separately. Retire deletes the
// merged branch, keeps the task branch with its reason, and reports
// each — the worktree's branch plus the task family.
func TestRetireDeletesMergedKeepsTaskFamily(t *testing.T) {
	_, clone, envDir := e2eRepo(t)
	ctx := context.Background()
	gitRun(t, clone, "checkout", "-q", "-b", "fix/AMUX-12-base-branch")
	gitRun(t, clone, "commit", "-q", "--allow-empty", "-m", "base work")
	gitRun(t, clone, "checkout", "-q", "main")
	gitRun(t, clone, "merge", "-q", "--ff-only", "fix/AMUX-12-base-branch")
	gitRun(t, clone, "push", "-q", "origin", "main")
	gitRun(t, clone, "checkout", "-q", "-b", "task/AMUX-12-work")
	gitRun(t, clone, "commit", "-q", "--allow-empty", "-m", "task work")
	gitRun(t, clone, "checkout", "-q", "main")
	wtPath := filepath.Join(filepath.Dir(clone), "wts", "wt-12")
	gitRun(t, clone, "worktree", "add", wtPath, "fix/AMUX-12-base-branch")
	// AGENTMUX_BRANCH is what `sessions create` recorded for the task
	// branch, even though the worktree ended up on the fix branch.
	writeRegistry(t, envDir, "task-agentmux-amux-12", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
		"AGENTMUX_BRANCH": "task/AMUX-12-work",
	})
	stopper := &fakeStopper{}
	res, err := Retire(ctx, DaemonEnv{Daemon: stopper}, "task-agentmux-amux-12", Options{})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if !res.BranchDeleted || res.Branch != "fix/AMUX-12-base-branch" {
		t.Errorf("result = %+v, want the merged fix branch deleted", res)
	}
	if res.BranchKept == "" || !strings.Contains(res.BranchKept, "task/AMUX-12-work") {
		t.Errorf("BranchKept = %q, want the task branch kept with a reason", res.BranchKept)
	}
	if len(res.Branches) != 2 {
		t.Fatalf("Branches = %+v, want both branches reported", res.Branches)
	}
	if branchExists(t, clone, "fix/AMUX-12-base-branch") {
		t.Error("merged branch still exists")
	}
	if !branchExists(t, clone, "task/AMUX-12-work") {
		t.Error("task branch ref is gone")
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree still on disk")
	}
}

// TestDetachedOnOriginRetires is the MERG-20 shape with nothing unique:
// a detached HEAD on origin retires the worktree with no branch fate.
func TestDetachedOnOriginRetires(t *testing.T) {
	_, clone, envDir := e2eRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(filepath.Dir(clone), "wts", "wt-detached")
	gitRun(t, clone, "worktree", "add", "--detach", wtPath)
	writeRegistry(t, envDir, "task-agentmux-amux-23", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	stopper := &fakeStopper{}
	res, err := Retire(ctx, DaemonEnv{Daemon: stopper}, "task-agentmux-amux-23", Options{})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if len(res.Branches) != 0 {
		t.Errorf("Branches = %+v, want none for a detached worktree", res.Branches)
	}
	if res.WorktreeKept != "" {
		t.Errorf("WorktreeKept = %q, want the worktree gone", res.WorktreeKept)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree still on disk")
	}
}

// TestDetachedUnpushedRefuses is the MERG-20 shape with unique work: a
// detached HEAD holding commits not on origin refuses the retire as
// invalid — removing the worktree would lose work no branch points at.
func TestDetachedUnpushedRefuses(t *testing.T) {
	_, clone, envDir := e2eRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(filepath.Dir(clone), "wts", "wt-detached-dirty")
	gitRun(t, clone, "worktree", "add", "--detach", wtPath)
	gitRun(t, wtPath, "commit", "-q", "--allow-empty", "-m", "detached work")
	writeRegistry(t, envDir, "task-agentmux-amux-23", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	stopper := &fakeStopper{}
	_, err := Retire(ctx, DaemonEnv{Daemon: stopper}, "task-agentmux-amux-23", Options{})
	if err == nil {
		t.Fatal("detached unpushed HEAD: nil error, want a refusal")
	}
	if ReasonOf(err) != safesend.ReasonInvalid {
		t.Errorf("reason = %s, want invalid", ReasonOf(err))
	}
	if !strings.Contains(DetailOf(err), "detached HEAD") {
		t.Errorf("detail = %q, want the detached HEAD named", DetailOf(err))
	}
	if len(stopper.calls) != 0 {
		t.Error("managed half ran despite the refusal")
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Error("worktree was removed despite the refusal")
	}
}

// TestRequireMergedRefusesUnmerged: the strict mode refuses the whole
// retire for unmerged branches — the old behavior, now opt-in — before
// touching anything.
func TestRequireMergedRefusesUnmerged(t *testing.T) {
	_, clone, envDir := e2eRepo(t)
	ctx := context.Background()
	gitRun(t, clone, "checkout", "-q", "-b", "task/AMUX-23-feat")
	gitRun(t, clone, "commit", "-q", "--allow-empty", "-m", "unique work")
	gitRun(t, clone, "checkout", "-q", "main")
	wtPath := filepath.Join(filepath.Dir(clone), "wts", "wt-feat")
	gitRun(t, clone, "worktree", "add", wtPath, "task/AMUX-23-feat")
	writeRegistry(t, envDir, "task-agentmux-amux-23", map[string]string{
		"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": wtPath,
	})
	stopper := &fakeStopper{}
	_, err := Retire(ctx, DaemonEnv{Daemon: stopper}, "task-agentmux-amux-23", Options{RequireMerged: true})
	if err == nil {
		t.Fatal("require-merged with unmerged work: nil error, want a refusal")
	}
	if ReasonOf(err) != safesend.ReasonInvalid {
		t.Errorf("reason = %s, want invalid", ReasonOf(err))
	}
	if !strings.Contains(DetailOf(err), "task/AMUX-23-feat") {
		t.Errorf("detail = %q, want the branch named", DetailOf(err))
	}
	if len(stopper.calls) != 0 {
		t.Error("managed half ran despite the refusal")
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Error("worktree was removed despite the refusal")
	}
	if !branchExists(t, clone, "task/AMUX-23-feat") {
		t.Error("branch ref is gone despite the refusal")
	}
}

// TestTaskKey extracts the task id from branches and instance names for
// the family glob.
func TestTaskKey(t *testing.T) {
	for in, want := range map[string]string{
		"task/AMUX-23-feat":         "AMUX-23",
		"task-agentmux-amux-23":     "AMUX-23",
		"fix/AMUX-12-base-branch":   "AMUX-12",
		"task/MERG-20-orchestrator": "MERG-20",
		"task-merge-queue":          "",
		"main":                      "",
		"task/AMUX-23":              "AMUX-23",
		"feature/no-id-here":        "",
	} {
		if got := taskKey(in); got != want {
			t.Errorf("taskKey(%q) = %q, want %q", in, got, want)
		}
	}
}
