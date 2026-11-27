package daemoninstall

import (
	"fmt"
	"os"
	"os/user"
)

const (
	binPath                  = "/usr/local/bin/agentmux"
	unitPath                 = "/etc/systemd/system/agentmuxd.service"
	doctorUnitPath           = "/etc/systemd/system/agentmuxd-doctor.service"
	doctorTimerPath          = "/etc/systemd/system/agentmuxd-doctor.timer"
	gcUnitPath               = "/etc/systemd/system/agentmuxd-gc.service"
	gcTimerPath              = "/etc/systemd/system/agentmuxd-gc.timer"
	taskcheckServiceUnitPath = "/etc/systemd/system/agentmux-taskcheck-broker.service"
	probeUnitPath            = "/etc/systemd/system/agentmuxd-probe.service"
	probeTimerPath           = "/etc/systemd/system/agentmuxd-probe.timer"
	daemonSocket             = "/run/agentmux/agentmuxd.sock"
	unitName                 = "agentmuxd.service"
	// Use agentmuxd-* rather than agentmux-* so these host jobs cannot
	// collide with the unit for an ordinary instance named "doctor".
	doctorUnitName       = "agentmuxd-doctor.service"
	doctorTimerName      = "agentmuxd-doctor.timer"
	gcUnitName           = "agentmuxd-gc.service"
	gcTimerName          = "agentmuxd-gc.timer"
	taskcheckServiceUnit = "agentmux-taskcheck-broker.service"
	probeTimerName       = "agentmuxd-probe.timer"
	unitTemplate         = `[Unit]
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

	gcUnitTemplate = `[Unit]
Description=agentmux retired-session garbage collection
After=agentmuxd.service network-online.target
Wants=network-online.target
Requires=agentmuxd.service

[Service]
Type=oneshot
ExecStart=%s gc -host local
TimeoutStartSec=15min
`

	gcTimerTemplate = `[Unit]
Description=Run agentmux gc daily

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
`
	// The provider probe runs as the operator user (User=), whose provider
	// logins it checks and whose state and usage file it writes. It starts
	// one runner process per try in a temporary directory and never an
	// instance (AMUX-139). The gateway reads the probe state of this User=.
	probeUnitTemplate = `[Unit]
Description=agentmux provider probe: one tiny request to each model provider
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=%s
ExecStart=%s models probe
TimeoutStartSec=20min
`

	probeTimerTemplate = `[Unit]
Description=Run the agentmux provider probe hourly

[Timer]
OnCalendar=hourly
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
`

	// The broker is wanted by the daemon rather than by multi-user.target:
	// it then starts at boot (the daemon is) and also whenever the daemon is
	// started again after a stop, which Requires= alone propagates only one
	// way. Without an [Install] section `enable` was a no-op and the broker
	// stayed down after every reboot.
	taskcheckServiceTemplate = `[Unit]
Description=agentmux task-check broker
After=agentmuxd.service
Requires=agentmuxd.service

[Service]
Type=simple
ExecStart=%s taskcheck-broker serve
Restart=on-failure
RestartSec=1
TimeoutStartSec=infinity
KillMode=process

[Install]
WantedBy=agentmuxd.service
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
	if err := InstallForDeploy(doctorTime); err != nil {
		return err
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
	if err := runCmd("systemctl", "enable", "--now", gcTimerName); err != nil {
		return err
	}
	if err := runCmd("systemctl", "enable", "--now", taskcheckServiceUnit); err != nil {
		return err
	}
	if err := runCmd("systemctl", "restart", taskcheckServiceUnit); err != nil {
		return err
	}
	if _, err := os.Stat(probeUnitPath); err == nil {
		if err := runCmd("systemctl", "enable", "--now", probeTimerName); err != nil {
			return err
		}
	}

	fmt.Printf("Installed and started %s plus %s at %s Australia/Perth plus %s (binary: %s, socket: %s)\n", unitName, doctorTimerName, doctorTime, gcTimerName, binPath, daemonSocket)
	return nil
}

// InstallForDeploy pins the current binary to binPath and rewrites the
// daemon, doctor and gc unit files without enabling or starting anything,
// so `agentmux deploy` can refresh every unit before restarting all the
// services itself (see AMUX-24). doctorTime is still parsed, so a bad
// value fails the same way Install would.
func InstallForDeploy(doctorTime string) error {
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
	gcUnit := fmt.Sprintf(gcUnitTemplate, binPath)
	if err := os.WriteFile(gcUnitPath, []byte(gcUnit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", gcUnitPath, err)
	}
	gcTimer := gcTimerTemplate
	if err := os.WriteFile(gcTimerPath, []byte(gcTimer), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", gcTimerPath, err)
	}
	if err := os.WriteFile(taskcheckServiceUnitPath, []byte(fmt.Sprintf(taskcheckServiceTemplate, binPath)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", taskcheckServiceUnitPath, err)
	}
	if u := ProbeRunUser(); u != "" {
		if err := os.WriteFile(probeUnitPath, []byte(fmt.Sprintf(probeUnitTemplate, u, binPath)), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", probeUnitPath, err)
		}
		if err := os.WriteFile(probeTimerPath, []byte(probeTimerTemplate), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", probeTimerPath, err)
		}
	} else {
		fmt.Printf("Skipped %s: run under sudo so the probe can run as the operator user\n", probeTimerName)
	}

	return nil
}

// ProbeTimerName is the hourly provider probe timer, for deploy to enable.
func ProbeTimerName() string { return probeTimerName }

// ProbeUnitInstalled reports whether the probe unit has been written.
func ProbeUnitInstalled() bool {
	_, err := os.Stat(probeUnitPath)
	return err == nil
}

// Uninstall stops and removes the unit. It leaves the installed binary at
// binPath in place, since it also serves as the TUI/wizard binary.
func Uninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux daemon uninstall")
	}
	_ = runCmd("systemctl", "disable", "--now", doctorTimerName)
	_ = runCmd("systemctl", "disable", "--now", gcTimerName)
	_ = runCmd("systemctl", "disable", "--now", taskcheckServiceUnit)
	_ = runCmd("systemctl", "disable", "--now", probeTimerName)
	_ = runCmd("systemctl", "disable", "--now", unitName)
	for _, path := range []string{probeTimerPath, probeUnitPath, taskcheckServiceUnitPath, gcTimerPath, gcUnitPath, doctorTimerPath, doctorUnitPath, unitPath} {
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
	gcActive := captureCmd("systemctl", "is-active", gcTimerName)
	gcEnabled := captureCmd("systemctl", "is-enabled", gcTimerName)
	probeActive := captureCmd("systemctl", "is-active", probeTimerName)
	probeEnabled := captureCmd("systemctl", "is-enabled", probeTimerName)
	return fmt.Sprintf("unit: %s\nactive: %s\nenabled: %s\nsocket: %s\n\ndoctor timer: %s\nactive: %s\nenabled: %s\n\ngc timer: %s\nactive: %s\nenabled: %s\n\nprobe timer: %s\nactive: %s\nenabled: %s", unitPath, active, enabled, daemonSocket, doctorTimerPath, doctorActive, doctorEnabled, gcTimerPath, gcActive, gcEnabled, probeTimerPath, probeActive, probeEnabled), nil
}

// ProbeRunUser is the operator user the probe runs as: SUDO_USER when
// install runs under sudo and names a real, non-root user; else "".
func ProbeRunUser() string {
	u := os.Getenv("SUDO_USER")
	if u == "" || u == "root" {
		return ""
	}
	if _, err := user.Lookup(u); err != nil {
		return ""
	}
	return u
}
