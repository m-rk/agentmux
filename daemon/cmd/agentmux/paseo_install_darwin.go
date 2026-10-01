//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, path).Run() // may not be loaded yet
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	fmt.Printf("Installed %s at %s local time (binary: %s)\n", paseoUpdateLabel, at, bin)
	return nil
}
