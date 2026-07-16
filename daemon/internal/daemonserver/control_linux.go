package daemonserver

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// applyControl runs the requested action against inst's systemd unit
// (inst.ServiceName).
func applyControl(ctx context.Context, inst discovery.Instance, action string) (bool, string) {
	out, err := exec.CommandContext(ctx, "systemctl", action, inst.ServiceName).CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("%s %s: %v: %s", action, inst.ServiceName, err, out)
	}
	return true, fmt.Sprintf("%s %s ok", action, inst.ServiceName)
}
