//go:build linux

package preview

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// pinnedRoot opens dir as an os.Root that stays bound to the directory that
// was validated. It opens the directory with O_NOFOLLOW, checks through
// /proc/self/fd that the descriptor really is dir, and then roots at that
// descriptor, so a run user who swaps a path component for a symlink after
// ResolveDir ran cannot redirect the broker to another directory.
func pinnedRoot(dir string) (*os.Root, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	link := fmt.Sprintf("/proc/self/fd/%d", fd)
	real, err := os.Readlink(link)
	if err != nil || filepath.Clean(real) != filepath.Clean(dir) {
		return nil, fmt.Errorf("folder changed while it was being opened")
	}
	return os.OpenRoot(link)
}
