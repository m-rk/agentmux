//go:build linux

package main

import (
	"fmt"
	"os"
)

// installGatewayService writes and starts the systemd unit. In print mode it
// only shows the unit and touches nothing, so it needs no root.
func installGatewayService(runUser, _ /*home*/, bin string, runArgs []string, print bool) error {
	unit := renderGatewayUnit(runUser, bin, runArgs)
	path := "/etc/systemd/system/" + gatewayServiceName
	if print {
		fmt.Printf("# %s\n%s", path, unit)
		return nil
	}
	if err := rewriteGatewayUnit(runUser, bin, runArgs); err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", gatewayServiceName); err != nil {
		return err
	}
	fmt.Printf("Installed and started %s (binary: %s, user: %s)\n", gatewayServiceName, bin, runUser)
	return nil
}

// rewriteGatewayUnit pins the unit file at path to the new binary and run
// args without enabling or starting anything, so `agentmux deploy` can
// refresh the unit before restarting the service itself (see AMUX-24).
func rewriteGatewayUnit(runUser, bin string, runArgs []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux gateway install -run-user %s ...", runUser)
	}
	unit := renderGatewayUnit(runUser, bin, runArgs)
	path := "/etc/systemd/system/" + gatewayServiceName
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
