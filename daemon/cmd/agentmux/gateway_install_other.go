//go:build !linux && !darwin

package main

import "fmt"

func installGatewayService(_, _, _ string, _ []string, _ bool) error {
	return fmt.Errorf("agentmux gateway install is only supported on Linux (systemd) and macOS (launchd)")
}
