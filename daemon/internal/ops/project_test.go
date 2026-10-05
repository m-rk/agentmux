package ops

import (
	"os/exec"
	"testing"
)

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestProjectOfFromOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := gitRepo(t, "https://github.com/owner/repo.git")
	if got := ProjectOf("no-such-instance", dir, nil); got != "github.com/owner/repo" {
		t.Fatalf("project = %q, want github.com/owner/repo", got)
	}
}

func TestProjectOfOverrideAndEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := gitRepo(t, "https://github.com/owner/repo.git")
	if got := ProjectOf("inst", dir, map[string]string{"inst": "acme/other"}); got != "acme/other" {
		t.Fatalf("override project = %q, want acme/other", got)
	}
	if got := ProjectOf("inst", t.TempDir(), nil); got != "" {
		t.Fatalf("non-repo project = %q, want empty", got)
	}
	if got := ProjectOf("inst", "", nil); got != "" {
		t.Fatalf("no workdir project = %q, want empty", got)
	}
}
