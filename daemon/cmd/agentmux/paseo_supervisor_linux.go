//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// findPaseoSupervisor looks for a systemd service unit that runs `paseo
// daemon ...`. There is no fixed name for it: paseo onboarding or a host's
// own setup can call the unit anything (paseo.service, paseo-daemon.service,
// ...), so every known service unit's ExecStart is checked by command rather
// than by name, the same way findPaseoSupervisor on macOS matches launchd
// jobs by ProgramArguments instead of by label.
func findPaseoSupervisor(ctx context.Context, _ /*home*/ string) (*paseoSupervisor, error) {
	names, err := paseoCandidateUnitNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		unit, err := exec.CommandContext(ctx, "systemctl", "cat", name).Output()
		if err != nil {
			continue // not loaded, or unreadable; not ours to judge
		}
		args := systemdExecStart(string(unit))
		if !isPaseoDaemonCommand(args) {
			continue
		}
		sup := &paseoSupervisor{Kind: "systemd unit", Name: name, Args: args}
		out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState", "-p", "SubState", name).Output()
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
	return nil, nil
}

// paseoCandidateUnitNames lists every service unit file systemd knows about,
// loaded or not, so a unit that exists but was never started (e.g. right
// after install) is still found.
func paseoCandidateUnitNames(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-unit-files", "--type=service", "--no-legend", "--no-pager").Output()
	if err != nil {
		return nil, fmt.Errorf("listing systemd service units: %w", err)
	}
	var names []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		names = append(names, fields[0])
	}
	return names, nil
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
