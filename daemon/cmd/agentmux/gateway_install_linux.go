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
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux gateway install -run-user %s ...", runUser)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
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
