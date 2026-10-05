// RetireInstance's managed half: stopping the session and removing its
// units and registry entry. These paths are privileged on Linux (the
// registry lives in root-owned /etc/agentmux, the units in
// /etc/systemd/system), so the root daemon owns them — not the
// unprivileged `sessions retire` caller, and never a root process doing
// git work. Both the daemon's RetireInstance RPC and the retire package's
// CLI-side flow call these; moving them here keeps one implementation
// behind both callers.
package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// artifactUnits lists every unit/timer the provisioner may have installed
// for an instance, on either platform.
func artifactUnits(instance string) []string {
	return []string{"agentmux-" + instance + ".service",
		"agentmux-" + instance + "-update.service",
		"agentmux-" + instance + "-update.timer",
		"agentmux-" + instance + "-tick.service",
		"agentmux-" + instance + "-tick.timer"}
}

// artifactLabels lists the macOS LaunchAgent labels for an instance.
func artifactLabels(instance string) []string {
	return []string{"com.agentmux." + instance, "com.agentmux." + instance + ".update"}
}

// StopManaged stops the instance's session. On Linux it goes through
// `systemctl stop`, so the unit's own ExecStop runs the agent shutdown
// as the unit user — never the caller's user. On macOS the LaunchAgent
// already runs as the instance's own user, so session.Stop directly.
func StopManaged(instance string) error {
	if runtime.GOOS == "linux" {
		if out, err := systemctlStop(instance); err != nil {
			return fmt.Errorf("stopping agentmux-%s.service: %v: %s", instance, err, out)
		}
		return nil
	}
	return Stop(instance)
}

// systemctlStop stops the instance's unit. A var so tests substitute a
// fake without touching the host's service manager.
var systemctlStop = func(instance string) ([]byte, error) {
	return exec.Command("systemctl", "stop", "agentmux-"+instance+".service").CombinedOutput()
}

// RemoveUnits disables and deletes every unit/timer/plist the provisioner
// may have installed for the instance, on either platform. Missing units
// are fine — an amp instance with self-updates off never had an update
// unit, and a macOS host has no systemd units at all.
func RemoveUnits(instance string) error {
	systemctl("disable", "--now", "agentmux-"+instance+".service")
	systemctl("disable", "--now", "agentmux-"+instance+"-update.service")
	systemctl("disable", "--now", "agentmux-"+instance+"-update.timer")
	systemctl("disable", "--now", "agentmux-"+instance+"-tick.service")
	systemctl("disable", "--now", "agentmux-"+instance+"-tick.timer")
	for _, unit := range artifactUnits(instance) {
		if err := os.Remove(filepath.Join("/etc/systemd/system", unit)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing unit %s: %w", unit, err)
		}
	}
	systemctl("daemon-reload")
	for _, label := range artifactLabels(instance) {
		launchctlBootout(label)
	}
	for _, label := range artifactLabels(instance) {
		home := runas.CurrentUserHome()
		if home == "" {
			continue
		}
		if err := os.Remove(filepath.Join(home, "Library", "LaunchAgents", label+".plist")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing plist %s: %w", label, err)
		}
	}
	return nil
}

// systemctl runs systemctl with args, ignoring the result: disabling a
// unit that was never installed is not an error. A var so tests
// substitute a fake without touching the host's service manager.
var systemctl = func(args ...string) {
	_ = exec.Command("systemctl", args...).Run()
}

// launchctlBootout unloads a per-user LaunchAgent label, ignoring the
// result. A var so tests substitute a fake on non-macOS hosts.
var launchctlBootout = func(label string) {
	home := runas.CurrentUserHome()
	if home == "" {
		return
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	_ = exec.Command("launchctl", "bootout", "gui/"+uidOf(), plist).Run()
}

func uidOf() string {
	return fmt.Sprint(os.Getuid())
}

// RemoveRegistry deletes the instance's registry file, so discovery stops
// listing it.
func RemoveRegistry(instance string) error {
	path := filepath.Join(discovery.EnvDir, instance+".env")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing registry for %s: %w", instance, err)
	}
	return nil
}
