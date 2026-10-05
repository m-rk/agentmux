//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
)

// deployOwnedServices lists the user services deploy restarts when their
// unit exists: the gateway, asks serve, thread watch and its nightly
// review. Instance units (agentmux-<name>.service) are never touched —
// deploy must not interrupt running agents. Timers (doctor, gc, review,
// paseo) are never restarted either: they run the pinned binary on their
// next tick, and enabling an already-enabled timer is enough.
func deployOwnedServices() []deployService {
	return []deployService{
		{unit: "agentmux-gateway.service", label: "gateway"},
		{unit: "agentmux-asks.service", label: "asks serve"},
		{unit: "agentmux-threadwatch.service", label: "threadwatch"},
	}
}

// systemctlRun runs systemctl, capturing combined output for errors.
func systemctlRun(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// systemctlShow returns selected unit properties (LoadState, ActiveState,
// SubState, User, FragmentPath), used to detect installed units and to
// restart services with the same user they already run as.
func systemctlShow(ctx context.Context, unit string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "show", unit,
		"--property=LoadState,ActiveState,SubState,User,FragmentPath")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	return props, nil
}

// deployServiceInstalled reports whether unit is installed (a unit file is
// loaded), so deploy skips services the operator never set up.
func deployServiceInstalled(ctx context.Context, unit string) bool {
	props, err := systemctlShow(ctx, unit)
	if err != nil {
		return false
	}
	return props["LoadState"] == "loaded"
}

// deployRestartServices restarts the daemon plus the installed owned
// services, waiting for each to become active. Timers (doctor, gc,
// threadwatch review, paseo) are left alone: they run the pinned binary
// on their next tick, and restarting a timer would only fire it now.
func deployRestartServices(ctx context.Context, services []deployService) error {
	if err := systemctlRun(ctx, "daemon-reload"); err != nil {
		return err
	}
	units := []string{"agentmuxd.service"}
	for _, s := range services {
		units = append(units, s.unit)
	}
	for _, unit := range units {
		if err := systemctlRun(ctx, "restart", unit); err != nil {
			return fmt.Errorf("restarting %s: %w", unit, err)
		}
		if err := deployWaitActive(ctx, unit, 30*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// deployWaitActive polls until unit reports active (or sub failed), so a
// restart that immediately crashes fails the deploy instead of passing
// the version check against the old process.
func deployWaitActive(ctx context.Context, unit string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		props, err := systemctlShow(ctx, unit)
		if err == nil {
			switch props["ActiveState"] {
			case "active":
				return nil
			case "failed", "inactive":
				return fmt.Errorf("%s is %s after restart (sub=%s)", unit, props["ActiveState"], props["SubState"])
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become active within %s", unit, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// deployServiceMainPID returns the service's main PID via systemctl, or 0
// when the unit has none (oneshot, or not actually running).
func deployServiceMainPID(ctx context.Context, unit string) uint32 {
	cmd := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=MainPID", "--value")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var pid uint32
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid)
	return pid
}

// deployBinaryStartTime returns the start time of pid as "start <pid>",
// used to confirm a restarted service is a new process. Empty when pid is
// unknown or already gone.
func deployBinaryStartTime(pid uint32) string {
	if pid == 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return ""
	}
	return "start " + strings.TrimSpace(string(out))
}

// deployDoctorTimeFromTimer reads the installed doctor timer's HH:MM, so
// deploy keeps the operator's schedule instead of resetting it to the
// default. Empty when no doctor timer is installed.
func deployDoctorTimeFromTimer(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "show", "agentmuxd-doctor.timer",
		"--property=TimersCalendar", "--value")
	out, err := cmd.Output()
	if err != nil {
		return "", nil // no timer installed: caller falls back to the default
	}
	// "{ OnCalendar=*-*-* 03:30:00 Australia/Perth ; next_elapse=... }".
	for _, field := range strings.Split(string(out), ";") {
		field = strings.Trim(strings.TrimSpace(field), "{}")
		cal, ok := strings.CutPrefix(strings.TrimSpace(field), "OnCalendar=")
		if !ok {
			continue
		}
		var hhmm string
		if _, err := fmt.Sscanf(strings.TrimSpace(cal), "*-*-* %5s", &hhmm); err == nil {
			hhmm = strings.TrimSuffix(hhmm, ":00")
			if _, _, err := daemoninstall.ParseDoctorTime(hhmm); err == nil {
				return hhmm, nil
			}
		}
	}
	return "", nil
}

// deployCheckRootOwnedRepos implements the AMUX-23 ownership guard: files
// the deploy wrote as root (unit files are root-owned by design) must not
// leak root ownership into a user's repo. It scans the template repo's
// .git for root-owned files left by git the deploy ran.
func deployCheckRootOwnedRepos(ctx context.Context, repo string) error {
	if os.Geteuid() != 0 || repo == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "find", repo+"/.git", "-uid", "0", "-print")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// find failing (no .git, no permission) is not a deploy failure;
		// the smoke test below would catch a truly broken repo.
		return nil
	}
	if files := strings.TrimSpace(string(out)); files != "" {
		first := strings.SplitN(files, "\n", 2)[0]
		n := strings.Count(files, "\n") + 1
		return fmt.Errorf("%d root-owned file(s) under %s/.git (e.g. %s): a privileged git step ran as root; see AMUX-23", n, repo, first)
	}
	return nil
}
