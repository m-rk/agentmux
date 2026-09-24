//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
)

const (
	reviewUnitPath  = "/etc/systemd/system/agentmux-threadwatch-review.service"
	reviewTimerPath = "/etc/systemd/system/agentmux-threadwatch-review.timer"
	reviewTimerName = "agentmux-threadwatch-review.timer"

	// reviewUnitTemplate runs the review as the operator user directly
	// (User=), unlike agentmuxd-doctor.service (which stays root and drops
	// privilege internally via runas): docs/design/thread-watch.md,
	// "Running it and secrets" says thread watch runs "as the operator
	// user, not inside root agentmuxd", so it can read the user's own
	// transcripts and Discord config without root.
	reviewUnitTemplate = `[Unit]
Description=agentmux threadwatch nightly review
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=%s
ExecStart=%s threadwatch review
TimeoutStartSec=15min
`

	// reviewTimerTemplate uses the same timezone convention as
	// agentmuxd-doctor.timer (internal/daemoninstall/install_linux.go).
	reviewTimerTemplate = `[Unit]
Description=Run the agentmux threadwatch nightly review

[Timer]
OnCalendar=*-*-* %s:00 Australia/Perth
Persistent=true

[Install]
WantedBy=timers.target
`
)

// installThreadwatchReviewTimer writes and enables
// agentmux-threadwatch-review.{service,timer}, running `agentmux
// threadwatch review` as runUser at local time at (HH:MM) daily. In print
// mode it only prints the two unit files and touches nothing on the
// system, so it works without root and is safe to preview.
func installThreadwatchReviewTimer(runUser, bin, at string, print bool) error {
	if runUser == "" {
		return fmt.Errorf("run-user is required")
	}
	if _, _, err := daemoninstall.ParseDoctorTime(at); err != nil {
		return fmt.Errorf("invalid -at time %q: %w", at, err)
	}

	unit := fmt.Sprintf(reviewUnitTemplate, runUser, bin)
	timer := fmt.Sprintf(reviewTimerTemplate, at)

	if print {
		fmt.Printf("# %s\n%s\n# %s\n%s\n", reviewUnitPath, unit, reviewTimerPath, timer)
		return nil
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("must be run as root; try: sudo agentmux threadwatch review install")
	}
	if err := os.WriteFile(reviewUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", reviewUnitPath, err)
	}
	if err := os.WriteFile(reviewTimerPath, []byte(timer), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", reviewTimerPath, err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", reviewTimerName); err != nil {
		return err
	}

	fmt.Printf("Installed and started %s at %s Australia/Perth (binary: %s, user: %s)\n", reviewTimerName, at, bin, runUser)
	return nil
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
