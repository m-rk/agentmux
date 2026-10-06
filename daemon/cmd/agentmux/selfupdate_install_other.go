//go:build !darwin

package main

import "fmt"

// installSelfUpdateSchedule reports unsupported off macOS: the pull
// updater (AMUX-29) targets the Mac host, where launchd keeps it alive.
// Linux deploys stay push-based via `agentmux deploy`.
func installSelfUpdateSchedule(_, _ string, _ selfUpdateHostConfig, _ bool) error {
	return fmt.Errorf("agentmux self-update install is only supported on macOS (launchd)")
}
