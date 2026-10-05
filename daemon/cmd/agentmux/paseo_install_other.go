//go:build !linux && !darwin

package main

import "fmt"

func installPaseoUpdateSchedule(_, _, _, _ string, _ bool) error {
	return fmt.Errorf("agentmux paseo update install is only supported on Linux (systemd) and macOS (launchd)")
}
