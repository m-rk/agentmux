package ops

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// codexGoCache creates the instance's private Go build cache and seeds it
// from the host's shared cache, returning its path ("" on a dry run or when
// the directory can't be made). Codex's sandbox keeps the shared cache
// read-only, so without this every run rebuilds the standard library into a
// throwaway cache. The copy uses reflinks, and
// never hard links, since a worker writing through
// one would alter the shared entry. Seeding is best-effort: an empty cache
// still works, just cold.
//
// Without reflinks a copy would put the whole shared cache (several GB) on
// disk for every task instance, so seeding is skipped there: the instance's
// cache starts cold and is warm from its second build on.
func (e Env) codexGoCache(instance string, dryRun bool) string {
	if dryRun {
		return ""
	}
	home := runas.CurrentUserHome()
	if home == "" {
		return ""
	}
	dir := session.CodexGoCacheDir(home, instance)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	if src := hubGoCache(home); src != "" {
		from := src + string(filepath.Separator) + "."
		if exec.Command("cp", "-a", "--reflink=always", from, dir).Run() != nil {
			// No reflinks (ext4, or no GNU cp): clear a partial copy and start cold.
			_ = os.RemoveAll(dir)
			_ = os.MkdirAll(dir, 0o700)
		}
	}
	return dir
}

// hubGoCache is the shared cache to seed from: the Go default location.
func hubGoCache(home string) string {
	src := filepath.Join(home, ".cache", "go-build")
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return ""
	}
	return src
}
