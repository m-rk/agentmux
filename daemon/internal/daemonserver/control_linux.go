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
	systemdAction := linuxSystemdAction(inst, action)
	out, err := exec.CommandContext(ctx, "systemctl", systemdAction, serviceName).CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("%s %s: %v: %s", systemdAction, serviceName, err, out)
	}
	if systemdAction != action {
		return true, fmt.Sprintf("%s %s ok via systemd %s", action, serviceName, systemdAction)
	}
	return true, fmt.Sprintf("%s %s ok", systemdAction, serviceName)
}

// A oneshot+RemainAfterExit unit stays "active" when its independently
// managed tmux session dies. systemctl start would therefore succeed without
// rerunning ExecStart. Restart has start semantics for an inactive unit and
// forces ExecStart for this active-but-dead case.
func linuxSystemdAction(inst discovery.Instance, action string) string {
	if action == "start" && inst.Status == discovery.StatusDead {
		return "restart"
	}
	return action
}
