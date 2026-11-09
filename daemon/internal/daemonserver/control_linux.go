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
	// The nightly update and the periodic health tick each start a session
	// that isn't running, so a stop only sticks if they are switched off with
	// it, and a start has to switch them back on (AMUX-129). Disabling the
	// main unit also keeps a stopped instance down across a reboot. Not every
	// agent has both timers, so a failure on a timer is not an error.
	timers := []string{"agentmux-" + inst.Name + "-update.timer", "agentmux-" + inst.Name + "-tick.timer"}
	if action == "start" {
		if out, err := systemctlRun(ctx, "enable", serviceName); err != nil {
			return false, fmt.Sprintf("enable %s: %v: %s", serviceName, err, out)
		}
		for _, t := range timers {
			_, _ = systemctlRun(ctx, "enable", "--now", t)
		}
	}
	systemdAction := linuxSystemdAction(inst, action)
	out, err := systemctlRun(ctx, systemdAction, serviceName)
	if err != nil {
		return false, fmt.Sprintf("%s %s: %v: %s", systemdAction, serviceName, err, out)
	}
	if action == "stop" {
		for _, t := range timers {
			_, _ = systemctlRun(ctx, "disable", "--now", t)
		}
		if out, err := systemctlRun(ctx, "disable", serviceName); err != nil {
			return false, fmt.Sprintf("stopped %s but could not disable it, so it may come back at the next restart: %v: %s", serviceName, err, out)
		}
		return true, fmt.Sprintf("stop %s ok (disabled, stays stopped until started)", serviceName)
	}
	if systemdAction != action {
		return true, fmt.Sprintf("%s %s ok via systemd %s", action, serviceName, systemdAction)
	}
	return true, fmt.Sprintf("%s %s ok", systemdAction, serviceName)
}

// systemctlRun is replaceable in tests.
var systemctlRun = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
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
