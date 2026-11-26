//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
)

// installPaseoUpdateSchedule writes and loads a per-user LaunchAgent. Like
// `agentmux daemon install` on macOS, it must not run as root: the paseo
// install, its daemon, and the Discord config are all per-user.
func installPaseoUpdateSchedule(_ /*runUser*/, home, bin, at string, print bool) error {
	logDir := filepath.Join(home, ".agentmux", "log")
	plist, err := renderPaseoUpdatePlist(bin, at, logDir)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", paseoUpdateLabel+".plist")
	if print {
		fmt.Printf("# %s\n%s", path, plist)
		return nil
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("must not be run as root/sudo on macOS; run as your normal user")
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("agentmux binary %s not found; run `agentmux daemon install` first or pass -bin", bin)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	// Only a changed plist is re-registered (AMUX-143).
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if _, err := launchagent.Apply(launchagent.Run, domain, path, []byte(plist)); err != nil {
		return err
	}
	fmt.Printf("Installed %s at %s local time (binary: %s)\n", paseoUpdateLabel, at, bin)
	return nil
}
