package main

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	paseoUpdateServiceName = "agentmux-paseo-update.service"
	paseoUpdateTimerName   = "agentmux-paseo-update.timer"
	paseoUpdateLabel       = "com.m-rk.agentmux.paseo-update"
)

// Linux: the job runs as root because restarting the system-level
// paseo-daemon.service needs it; npm and paseo themselves are run as runUser
// (internal/runas), so the global install stays owned by that user.
const paseoUpdateServiceTemplate = `[Unit]
Description=agentmux Paseo daemon update (install latest, verify, roll back on failure)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%s paseo update -run-user %s
TimeoutStartSec=10min
`

const paseoUpdateTimerTemplate = `[Unit]
Description=Check for a new Paseo daemon release daily

[Timer]
OnCalendar=*-*-* %s:00 Australia/Perth
Persistent=true

[Install]
WantedBy=timers.target
`

const paseoUpdatePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>paseo</string>
        <string>update</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>%d</integer>
        <key>Minute</key>
        <integer>%d</integer>
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s/paseo-update.log</string>
    <key>StandardErrorPath</key>
    <string>%s/paseo-update.err.log</string>
</dict>
</plist>
`

func renderPaseoUpdateUnits(runUser, bin, at string) (service, timer string) {
	return fmt.Sprintf(paseoUpdateServiceTemplate, bin, runUser), fmt.Sprintf(paseoUpdateTimerTemplate, at)
}

func renderPaseoUpdatePlist(bin, at, logDir string) (string, error) {
	hour, minute, ok := strings.Cut(at, ":")
	if !ok {
		return "", fmt.Errorf("invalid time %q (want HH:MM)", at)
	}
	h, err1 := strconv.Atoi(hour)
	m, err2 := strconv.Atoi(minute)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("invalid time %q (want HH:MM)", at)
	}
	return fmt.Sprintf(paseoUpdatePlistTemplate, paseoUpdateLabel, bin, h, m, logDir, logDir), nil
}
