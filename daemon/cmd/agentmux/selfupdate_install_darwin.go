//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// installSelfUpdateSchedule writes and loads the per-user LaunchAgent.
// Like `agentmux daemon install` on macOS, it must not run as root: the
// checkouts, bins and state are all per-user.
func installSelfUpdateSchedule(home, bin string, cfg selfUpdateHostConfig, print bool) error {
	goBin, err := resolveSelfUpdateGo(cfg.GoBin, os.Getenv("PATH"))
	if err != nil {
		return fmt.Errorf("self-update install: %w", err)
	}
	cfg.GoBin = goBin
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
	// Only a changed plist is re-registered (AMUX-143).
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if _, err := launchagent.Apply(launchagent.Run, domain, path, []byte(plist)); err != nil {
		return err
	}
	fmt.Printf("Installed %s every %d seconds (binary: %s, state: %s)\n", selfUpdateLabel, selfUpdateInterval, bin, selfupdate.BaseDir(home))
	return nil
}
