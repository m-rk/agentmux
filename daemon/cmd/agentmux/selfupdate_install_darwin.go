//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// installSelfUpdateSchedule writes and loads the per-user LaunchAgent.
// Like `agentmux daemon install` on macOS, it must not run as root: the
// checkouts, bins and state are all per-user.
func installSelfUpdateSchedule(home, bin string, cfg selfUpdateHostConfig, print bool) error {
	logDir := filepath.Join(home, ".agentmux", "log")
	plist := renderSelfUpdatePlist(bin, cfg, logDir)
	path := filepath.Join(home, "Library", "LaunchAgents", selfUpdateLabel+".plist")
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
	for _, dir := range selfupdate.Dirs(home) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, path).Run() // may not be loaded yet
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	fmt.Printf("Installed %s every %d seconds (binary: %s, state: %s)\n", selfUpdateLabel, selfUpdateInterval, bin, selfupdate.BaseDir(home))
	return nil
}
