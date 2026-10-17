//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const gatewayLaunchdLabel = "com.m-rk.agentmux.gateway"

func gatewayLaunchdProblem(home string) string {
	plist := filepath.Join(home, "Library", "LaunchAgents", gatewayLaunchdLabel+".plist")
	if _, err := os.Stat(plist); err != nil {
		return ""
	}
	out, _ := exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+gatewayLaunchdLabel).CombinedOutput()
	if failure := launchdFailure(string(out)); failure == "" {
		return ""
	} else if failure == "not running, last exit code 78" {
		return "agentmux gateway LaunchAgent exited with code 78; re-register it with `agentmux gateway install -capability <configured-capability>`"
	}
	return ""
}
