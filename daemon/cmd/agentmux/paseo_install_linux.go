//go:build linux

package main

import (
	"fmt"
	"os"
)

// installPaseoUpdateSchedule writes and enables the systemd timer. In print
// mode it only shows the units and touches nothing, so it needs no root.
func installPaseoUpdateSchedule(runUser, _ /*home*/, bin, at string, print bool) error {
	service, timer := renderPaseoUpdateUnits(runUser, bin, at)
	servicePath := "/etc/systemd/system/" + paseoUpdateServiceName
	timerPath := "/etc/systemd/system/" + paseoUpdateTimerName
	if print {
		fmt.Printf("# %s\n%s\n# %s\n%s\n", servicePath, service, timerPath, timer)
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux paseo update install -run-user %s", runUser)
	}
	if err := os.WriteFile(servicePath, []byte(service), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", servicePath, err)
	}
	if err := os.WriteFile(timerPath, []byte(timer), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", timerPath, err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", paseoUpdateTimerName); err != nil {
		return err
	}
	fmt.Printf("Installed and started %s at %s Australia/Perth (binary: %s, user: %s)\n", paseoUpdateTimerName, at, bin, runUser)
	return nil
}
