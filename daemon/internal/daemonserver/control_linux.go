package daemonserver

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// applyControl runs the requested action against the instance's managed unit.
// Derive the unit name instead of trusting mutable registry content to name an
// arbitrary systemd service for the root daemon to control.
func applyControl(ctx context.Context, inst discovery.Instance, action string) (bool, string) {
	serviceName := "agentmux-" + inst.Name + ".service"
	out, err := exec.CommandContext(ctx, "systemctl", action, serviceName).CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("%s %s: %v: %s", action, serviceName, err, out)
	}
	return true, fmt.Sprintf("%s %s ok", action, serviceName)
}
