//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// installGatewayService writes and loads a per-user LaunchAgent that keeps
// `agentmux gateway run` alive. Like `agentmux daemon install` on macOS, it
// must not run as root: the sessions and the tailscale CLI are per-user.
func installGatewayService(_ /*runUser*/, home, bin string, runArgs []string, print bool) error {
	logDir := filepath.Join(home, ".agentmux", "log")
	plist := renderGatewayPlist(bin, runArgs, logDir)
	path := filepath.Join(home, "Library", "LaunchAgents", gatewayLabel+".plist")
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
	fmt.Printf("Installed %s (binary: %s, logs: %s)\n", gatewayLabel, bin, logDir)
	return nil
}
