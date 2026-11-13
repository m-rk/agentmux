//go:build !linux

package preview

import "os"

func pinnedRoot(dir string) (*os.Root, error) { return os.OpenRoot(dir) }
