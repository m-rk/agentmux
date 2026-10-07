package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallTrailerHookStripsTrailers is the AMUX-64 scratch-repo test:
// Create installs a commit-msg hook in the task worktree, a commit carrying
// agent trailers lands without them, and the hook logs one line.
func TestInstallTrailerHookStripsTrailers(t *testing.T) {
	c := newCreateEnv(t)
	// The fixture repo has no origin; point it at a non-go1com remote so
	// the hook installs (go1com repos keep stock behaviour).
	git(t, c.repo, "remote", "add", "origin", "https://github.com/m-rk/agentmux.git")
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(c.wtRoot, "task-1")

	// The hook lives in the worktree's own git dir, never the shared one.
	wtGitDir := git(t, wt, "rev-parse", "--absolute-git-dir")
	hookPath := filepath.Join(wtGitDir, "hooks", "commit-msg")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("reading installed hook: %v", err)
	}
	if !strings.Contains(string(data), "agentmux-commit-msg") {
		t.Fatalf("hook does not look agentmux-managed:\n%s", data)
	}
	if fi, err := os.Stat(hookPath); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("hook %s is not executable (mode %o)", hookPath, fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(c.repo, ".git", "hooks", "commit-msg")); !os.IsNotExist(err) {
		t.Fatalf("shared hooks dir gained a commit-msg (err=%v)", err)
	}
	if got := git(t, wt, "config", "--get", "core.hooksPath"); got != filepath.Join(wtGitDir, "hooks") {
		t.Fatalf("worktree core.hooksPath = %q", got)
	}
	// The main checkout keeps stock behaviour: no worktree-scoped key.
	if out, _ := c.env.git(context.Background(), c.repo, "config", "--worktree", "--get", "core.hooksPath"); out != "" {
		t.Fatalf("template worktree-scope core.hooksPath = %q, want empty", out)
	}

	// A commit carrying every trailer shape lands without them.
	msg := "AMUX-64: do the thing\n\nSome body.\n\nAmp-Thread-ID: https://ampcode.com/threads/T-1\nCo-authored-by: Amp <amp@ampcode.com>\nCo-Authored-By: Claude <noreply@anthropic.com>\nClaude-Session: abc123\nGenerated-With: x\nGenerated with Claude Code\n"
	commitOut := gitCapture(t, wt, "commit", "--allow-empty", "-m", msg)
	_ = commitOut
	got := git(t, wt, "log", "-1", "--format=%B")
	for _, trailer := range []string{"Amp-Thread-ID", "Co-authored-by", "Co-Authored-By", "Claude-Session", "Generated-With", "Generated with"} {
		if strings.Contains(got, trailer) {
			t.Errorf("committed message still carries %q:\n%s", trailer, got)
		}
	}
	if !strings.Contains(got, "AMUX-64: do the thing") || !strings.Contains(got, "Some body.") {
		t.Errorf("hook ate non-trailer content:\n%s", got)
	}
}

// TestInstallTrailerHookSkipsGo1com checks the MERG-7 opt-in: a repo whose
// origin lives under the go1com org gets no hook and no worktree-scoped
// key, so trailers there keep working.
func TestInstallTrailerHookSkipsGo1com(t *testing.T) {
	c := newCreateEnv(t)
	git(t, c.repo, "remote", "add", "origin", "https://github.com/go1com/somerepo.git")
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(c.wtRoot, "task-1")
	if _, err := os.Stat(filepath.Join(c.repo, ".git", "hooks", "commit-msg")); !os.IsNotExist(err) {
		t.Fatalf("go1com repo gained a hook (err=%v)", err)
	}
	if out, _ := c.env.git(context.Background(), wt, "config", "--get", "core.hooksPath"); out != "" {
		t.Fatalf("go1com worktree core.hooksPath = %q, want empty", out)
	}
}

// TestInstallTrailerHookPreservesExistingHook checks the chaining: a repo
// with its own commit-msg hook keeps running it from the worktree (copied
// to the worktree hooks dir as commit-msg.local), the agentmux entry
// point runs it first, and both run on commit.
func TestInstallTrailerHookPreservesExistingHook(t *testing.T) {
	c := newCreateEnv(t)
	git(t, c.repo, "remote", "add", "origin", "https://github.com/m-rk/agentmux.git")
	hooksDir := filepath.Join(c.repo, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "local-ran")
	prior := "#!/bin/sh\necho ran >> " + marker + "\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "commit-msg"), []byte(prior), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(c.wtRoot, "task-1")
	wtGitDir := git(t, wt, "rev-parse", "--absolute-git-dir")
	if _, err := os.Stat(filepath.Join(wtGitDir, "hooks", "commit-msg.local")); err != nil {
		t.Fatalf("pre-existing hook was not preserved as commit-msg.local: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "commit-msg.local")); !os.IsNotExist(err) {
		t.Fatalf("shared hooks dir gained a commit-msg.local (err=%v)", err)
	}
	git(t, wt, "commit", "--allow-empty", "-m", "x")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("pre-existing hook did not run on commit: %v", err)
	}
	// A rerun keeps the preserved hook instead of nesting preserves.
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wtGitDir, "hooks", "commit-msg.local.local")); !os.IsNotExist(err) {
		t.Errorf("rerun nested the preserve (commit-msg.local.local exists)")
	}
}

// TestCheckBranchTrailersNamesTheCommit seeds a branch with one clean and
// one trailer-carrying commit and checks the ship-side scan names exactly
// the dirty one.
func TestCheckBranchTrailersNamesTheCommit(t *testing.T) {
	c := newCreateEnv(t)
	_, pusher := originFor(t, c)
	// Branch off main in the pusher, push it to origin: one clean commit,
	// one with trailers.
	git(t, pusher, "checkout", "-q", "-b", "feature/trail")
	git(t, pusher, "commit", "-q", "--allow-empty", "-m", "clean work")
	git(t, pusher, "commit", "-q", "--allow-empty", "-m", "dirty work\n\nAmp-Thread-ID: https://ampcode.com/threads/T-9")
	git(t, pusher, "push", "-q", "origin", "feature/trail")
	dirty := git(t, pusher, "rev-parse", "HEAD")
	// The fixture origin is a local path, which has no GitHub org: point
	// the repo at a non-go1com remote first (the hook install needs an
	// origin too), then restore the local origin for the fetch-based
	// scan. projectOrgOfOrigin only affects the go1com skip, not the
	// git fetch itself.
	git(t, c.repo, "remote", "add", "upstream", "https://github.com/m-rk/agentmux.git")

	r := c.req()
	r.Base = "main"
	r.Branch = "feature/trail"
	if _, err := c.env.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	bad, err := c.env.CheckBranchTrailers(context.Background(), c.repo, "feature/trail", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0].Hash != dirty {
		t.Fatalf("trailer scan = %+v, want exactly the dirty commit %s", bad, dirty)
	}
	// A go1com origin reports nothing, even with the trailer present.
	git(t, c.repo, "remote", "set-url", "origin", "https://github.com/go1com/other.git")
	if bad, err := c.env.CheckBranchTrailers(context.Background(), c.repo, "feature/trail", "main"); err != nil || len(bad) != 0 {
		t.Fatalf("go1com scan = %+v, %v; want nil, nil", bad, err)
	}
}

// TestMessageHasTrailer covers the trailer shapes and the near-misses: a
// subject that merely mentions a trailer name is not flagged.
func TestMessageHasTrailer(t *testing.T) {
	for _, msg := range []string{
		"x\n\nAmp-Thread-ID: https://ampcode.com/threads/T-1",
		"x\n\nCo-authored-by: Amp <amp@ampcode.com>",
		"x\n\nCo-Authored-By: Claude <n@x>",
		"x\n\nclaude-session: abc",
		"x\n\nGenerated-With: Claude",
		"x\n\nGenerated with Claude Code",
		"  AMP-THREAD-ID:  x",
	} {
		if !messageHasTrailer(msg) {
			t.Errorf("messageHasTrailer(%q) = false, want true", msg)
		}
	}
	for _, msg := range []string{
		"AMUX-1: fix the Amp-Thread-ID parser\n\nBody.",
		"mention Co-Authored-By in passing",
		"x\n\nSigned-off-by: Alice <a@x>",
		"",
	} {
		if messageHasTrailer(msg) {
			t.Errorf("messageHasTrailer(%q) = true, want false", msg)
		}
	}
}

// TestScanWorktreePrivacyFlagsLeaks stages a diff with a personal path and
// a private hostname, plus an untracked file, and checks each is reported
// once while the /home/alice placeholder stays quiet.
func TestScanWorktreePrivacyFlagsLeaks(t *testing.T) {
	c := newCreateEnv(t)
	git(t, c.repo, "remote", "add", "origin", "https://github.com/m-rk/agentmux.git")
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(c.wtRoot, "task-1")
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("leak.txt", "config at /home/bob/.config/thing\n")
	write("host.txt", "dial build-box.tailabc123.ts.net tonight\n")
	write("ok.txt", "placeholder /home/alice/src is fine\n")
	write("untracked.txt", "note from laptop7.local about it\n")
	git(t, wt, "add", "leak.txt", "host.txt", "ok.txt")
	findings := c.env.ScanWorktreePrivacy(context.Background(), wt, 20)
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"leak.txt", "host.txt", "untracked.txt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings lack %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ok.txt") {
		t.Errorf("placeholder /home/alice/ flagged:\n%s", joined)
	}
}

// gitCapture runs git args in dir with the test identity.
func gitCapture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return git(t, dir, args...)
}
