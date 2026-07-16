package daemoninstall

import (
	"fmt"
	"os"
)

const (
	binPath      = "/usr/local/bin/agentmux"
	unitPath     = "/etc/systemd/system/agentmuxd.service"
	daemonSocket = "/run/agentmux/agentmuxd.sock"
	unitName     = "agentmuxd.service"
	unitTemplate = `[Unit]
Description=agentmux daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s daemon run -socket %s
Restart=on-failure
RuntimeDirectory=agentmux
RuntimeDirectoryMode=0755

[Install]
WantedBy=multi-user.target
`
)

// SocketPath returns the Unix socket the installed daemon listens on,
// so the TUI/wizard client can default to the right path without a
// hosts.yaml (see install_darwin.go's SocketPath for the macOS default).
func SocketPath() string {
	return daemonSocket
}

// Install renders and enables agentmuxd.service, pointing it at a stable
// copy of the current binary under /usr/local/bin. Requires root, since the
// unit (and the agentmux-<instance>.service units it will manage) are
// system-scoped, matching backends/*/install.sh's existing root requirement.
func Install() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux daemon install")
	}

	if err := installSelf(binPath); err != nil {
		return fmt.Errorf("installing binary to %s: %w", binPath, err)
	}

	unit := fmt.Sprintf(unitTemplate, binPath, daemonSocket)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd("systemctl", "enable", "--now", unitName); err != nil {
		return err
	}

	fmt.Printf("Installed and started %s (binary: %s, socket: %s)\n", unitName, binPath, daemonSocket)
	return nil
}

// Uninstall stops and removes the unit. It leaves the installed binary at
// binPath in place, since it also serves as the TUI/wizard binary.
func Uninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux daemon uninstall")
	}
	_ = runCmd("systemctl", "disable", "--now", unitName)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", unitPath, err)
	}
	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Removed %s (left %s in place)\n", unitName, binPath)
	return nil
}

func Status() (string, error) {
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Sprintf("not installed (no %s)", unitPath), nil
	}
	active := captureCmd("systemctl", "is-active", unitName)
	enabled := captureCmd("systemctl", "is-enabled", unitName)
	return fmt.Sprintf("unit: %s\nactive: %s\nenabled: %s\nsocket: %s", unitPath, active, enabled, daemonSocket), nil
}
