package main

import "fmt"

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
// Shared across platforms so tests assert the template everywhere; only
// installing it is darwin-gated.
func renderSelfUpdatePlist(bin string, cfg selfUpdateHostConfig, logDir string) string {
	return fmt.Sprintf(selfUpdatePlistTemplate, selfUpdateLabel,
		bin, "self-update", "run",
		cfg.AgentmuxURL, cfg.MergenticURL, cfg.AgentmuxDir, cfg.MergenticDir,
		cfg.AgentmuxBin, cfg.MergenticBin, cfg.AgentsBinDir, cfg.GatewaySocket,
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
        <key>AGENTMUX_SELF_UPDATE_AGENTMUX_BIN</key>
        <string>%s</string>
        <key>AGENTMUX_SELF_UPDATE_MERGENTIC_BIN</key>
        <string>%s</string>
        <key>AGENTMUX_SELF_UPDATE_AGENTS_BIN_DIR</key>
        <string>%s</string>
        <key>AGENTMUX_SELF_UPDATE_SOCKET</key>
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
