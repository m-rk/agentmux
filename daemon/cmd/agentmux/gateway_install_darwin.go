//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
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
	// Only a changed plist is re-registered (AMUX-143); an unchanged one is
	// kickstarted so the gateway still restarts on the current binary.
	domain := "gui/" + strconv.Itoa(os.Getuid())
	changed, err := launchagent.Apply(launchagent.Run, domain, path, []byte(plist))
	if err != nil {
		return err
	}
	if !changed {
		if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+gatewayLabel).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl kickstart: %w: %s", err, out)
		}
	}
	fmt.Printf("Installed %s (binary: %s, logs: %s)\n", gatewayLabel, bin, logDir)
	return nil
}
