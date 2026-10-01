//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// findPaseoSupervisor reports the paseo-daemon systemd unit, if installed.
func findPaseoSupervisor(ctx context.Context, _ /*home*/ string) (*paseoSupervisor, error) {
	unit, err := exec.CommandContext(ctx, "systemctl", "cat", paseoUnit).Output()
	if err != nil {
		return nil, nil // no such unit
	}
	sup := &paseoSupervisor{Kind: "systemd unit", Name: paseoUnit, Args: systemdExecStart(string(unit))}
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState", "-p", "SubState", paseoUnit).Output()
	if err == nil {
		props := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				props[k] = v
			}
		}
		active, sub := props["ActiveState"], props["SubState"]
		if active == "failed" || sub == "auto-restart" {
			sup.Failing = active + "/" + sub
		}
	}
	return sup, nil
}

func restartPaseoSupervisor(ctx context.Context, s *paseoSupervisor) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s is a system unit; run as root (the installed timer does)", s.Name)
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", s.Name).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart %s: %w: %s", s.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
