package discovery

import (
	"os"
	"path/filepath"
)

// defaultEnvDir is where instances register on macOS: a per-user directory,
// since macOS instances run as user-level LaunchAgents (no root), unlike
// Linux's root-owned /etc/agentmux. Falls back to a relative path if the
// home directory can't be resolved, matching os.UserHomeDir's own
// documented failure behavior elsewhere in this codebase.
func defaultEnvDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agentmux/registry"
	}
	return filepath.Join(home, ".agentmux", "registry")
}
