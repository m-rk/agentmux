package main

import (
	"fmt"
	"html"
	"strings"
)

const (
	gatewayServiceName = "agentmux-gateway.service"
	gatewayLabel       = "com.m-rk.agentmux.gateway"
)

// Linux: runs as the operator user, not root. That user needs the agentmuxd
// socket and `tailscale whois` (tailscale set --operator=USER); see
// docs/gateway.md.
const gatewayUnitTemplate = `[Unit]
Description=agentmux gateway (tailnet API for this host's agent sessions)
After=network-online.target agentmuxd.service tailscaled.service
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

const gatewayPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s/gateway.log</string>
    <key>StandardErrorPath</key>
    <string>%s/gateway.err.log</string>
</dict>
</plist>
`

// systemdQuote quotes one ExecStart word when it needs it.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

func renderGatewayUnit(runUser, bin string, runArgs []string) string {
	words := []string{systemdQuote(bin)}
	for _, a := range runArgs {
		words = append(words, systemdQuote(a))
	}
	return fmt.Sprintf(gatewayUnitTemplate, runUser, strings.Join(words, " "))
}

func renderGatewayPlist(bin string, runArgs []string, logDir string) string {
	var args strings.Builder
	for _, a := range append([]string{bin}, runArgs...) {
		fmt.Fprintf(&args, "        <string>%s</string>\n", html.EscapeString(a))
	}
	return fmt.Sprintf(gatewayPlistTemplate, gatewayLabel, args.String(), logDir, logDir)
}
