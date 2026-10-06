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

// selfUpdateLabel is the per-user LaunchAgent that pulls new main every
// 10 minutes (AMUX-29). The com.m-rk namespace keeps it beside the
// doctor and gc agents, away from instance labels (com.agentmux.<name>).
const selfUpdateLabel = "com.m-rk.agentmux.self-update"

// selfUpdateInterval is the launchd StartInterval in seconds: 10 minutes,
// so a merge to main reaches the Mac within 10 minutes.
const selfUpdateInterval = 600

// renderSelfUpdatePlist renders the updater LaunchAgent: it execs the
// pinned agentmux binary's own `self-update run`, with the repo URLs and
// install paths from the host config baked in as env (no personal paths
// in the repo; configured on the host by `self-update install`).
func renderSelfUpdatePlist(bin string, cfg selfUpdateHostConfig, logDir string) string {
	return fmt.Sprintf(selfUpdatePlistTemplate, selfUpdateLabel,
		bin, "self-update", "run",
		cfg.AgentmuxURL, cfg.MergenticURL, cfg.AgentmuxDir, cfg.MergenticDir,
		selfUpdateInterval, logDir, logDir)
}

const selfUpdatePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>%s</string>
        <string>%s</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>AGENTMUX_SELF_UPDATE_AGENTMUX_URL</key>
        <string>%s</string>
        <key>AGENTMUX_SELF_UPDATE_MERGENTIC_URL</key>
        <string>%s</string>
        <key>AGENTMUX_SELF_UPDATE_AGENTMUX_DIR</key>
        <string>%s</string>
        <key>AGENTMUX_SELF_UPDATE_MERGENTIC_DIR</key>
        <string>%s</string>
    </dict>
    <key>StartInterval</key>
    <integer>%d</integer>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s/self-update.log</string>
    <key>StandardErrorPath</key>
    <string>%s/self-update.err.log</string>
</dict>
</plist>
`

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
