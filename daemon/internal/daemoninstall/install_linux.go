package daemoninstall

import (
	"fmt"
	"os"
)

const (
	binPath         = "/usr/local/bin/agentmux"
	unitPath        = "/etc/systemd/system/agentmuxd.service"
	doctorUnitPath  = "/etc/systemd/system/agentmuxd-doctor.service"
	doctorTimerPath = "/etc/systemd/system/agentmuxd-doctor.timer"
	daemonSocket    = "/run/agentmux/agentmuxd.sock"
	unitName        = "agentmuxd.service"
	// Use agentmuxd-* rather than agentmux-* so these host jobs cannot
	// collide with the unit for an ordinary instance named "doctor".
	doctorUnitName  = "agentmuxd-doctor.service"
	doctorTimerName = "agentmuxd-doctor.timer"
	unitTemplate    = `[Unit]
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
	doctorUnitTemplate = `[Unit]
Description=agentmux session doctor
After=agentmuxd.service network-online.target
Wants=network-online.target
Requires=agentmuxd.service

[Service]
Type=oneshot
ExecStart=%s doctor
TimeoutStartSec=15min
`
	doctorTimerTemplate = `[Unit]
Description=Run agentmux doctor after the daily refresh window

[Timer]
OnCalendar=*-*-* %s:00 Australia/Perth
Persistent=true

[Install]
WantedBy=timers.target
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
func Install(doctorTime string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux daemon install")
	}
	if _, _, err := ParseDoctorTime(doctorTime); err != nil {
		return err
	}

	if err := installSelf(binPath); err != nil {
		return fmt.Errorf("installing binary to %s: %w", binPath, err)
	}

	unit := fmt.Sprintf(unitTemplate, binPath, daemonSocket)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}
	doctorUnit := fmt.Sprintf(doctorUnitTemplate, binPath)
	if err := os.WriteFile(doctorUnitPath, []byte(doctorUnit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", doctorUnitPath, err)
	}
	doctorTimer := fmt.Sprintf(doctorTimerTemplate, doctorTime)
	if err := os.WriteFile(doctorTimerPath, []byte(doctorTimer), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", doctorTimerPath, err)
	}

	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd("systemctl", "enable", "--now", unitName); err != nil {
		return err
	}
	if err := runCmd("systemctl", "enable", "--now", doctorTimerName); err != nil {
		return err
	}

	fmt.Printf("Installed and started %s plus %s at %s Australia/Perth (binary: %s, socket: %s)\n", unitName, doctorTimerName, doctorTime, binPath, daemonSocket)
	return nil
}

// Uninstall stops and removes the unit. It leaves the installed binary at
// binPath in place, since it also serves as the TUI/wizard binary.
func Uninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux daemon uninstall")
	}
	_ = runCmd("systemctl", "disable", "--now", doctorTimerName)
	_ = runCmd("systemctl", "disable", "--now", unitName)
	for _, path := range []string{doctorTimerPath, doctorUnitPath, unitPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
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
	doctorActive := captureCmd("systemctl", "is-active", doctorTimerName)
	doctorEnabled := captureCmd("systemctl", "is-enabled", doctorTimerName)
	return fmt.Sprintf("unit: %s\nactive: %s\nenabled: %s\nsocket: %s\n\ndoctor timer: %s\nactive: %s\nenabled: %s", unitPath, active, enabled, daemonSocket, doctorTimerPath, doctorActive, doctorEnabled), nil
}
