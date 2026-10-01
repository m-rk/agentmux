package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// paseoSupervisor is the OS service that keeps the Paseo daemon running: the
// paseo-daemon systemd unit on Linux, or a LaunchAgent running `paseo daemon`
// on macOS. A nil *paseoSupervisor means nothing supervises it (the paseo CLI
// daemonized it, or the desktop app runs it).
type paseoSupervisor struct {
	Kind    string   // "systemd unit" or "LaunchAgent"
	Name    string   // unit name or launchd label
	Args    []string // the command it runs
	Failing string   // set when the service manager reports it failing
}

// problem describes a supervisor that cannot be keeping the daemon up, or ""
// when there is nothing wrong (including no supervisor at all).
func (s *paseoSupervisor) problem() string {
	if s == nil {
		return ""
	}
	for _, arg := range s.Args {
		if arg == "--foreground" {
			return fmt.Sprintf("Paseo %s %s runs `paseo daemon start --foreground`, which Paseo 0.10 removed; change it to `paseo daemon run --home <paseo home>`", s.Kind, s.Name)
		}
	}
	if s.Failing != "" {
		return fmt.Sprintf("Paseo %s %s is failing (%s)", s.Kind, s.Name, s.Failing)
	}
	return ""
}

// paseoSupervisorProblem is problem() for this host, folding a lookup
// failure into the description so callers have one string to report.
func paseoSupervisorProblem(ctx context.Context, home string) string {
	sup, err := findPaseoSupervisor(ctx, home)
	if err != nil {
		return "checking the Paseo daemon's supervisor: " + err.Error()
	}
	return sup.problem()
}

// isPaseoDaemonCommand matches argv that runs `paseo daemon ...`, directly or
// through a wrapper such as /usr/bin/env or node.
func isPaseoDaemonCommand(args []string) bool {
	for i, arg := range args {
		if filepath.Base(arg) == "paseo" && i+1 < len(args) && args[i+1] == "daemon" {
			return true
		}
	}
	return false
}

// systemdExecStart pulls the ExecStart argv out of `systemctl cat` output,
// dropping systemd's executable prefixes (-, @, +, !, :).
func systemdExecStart(unit string) []string {
	var args []string
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok || rest == "" {
			continue
		}
		args = strings.Fields(strings.TrimLeft(rest, "-@+!:"))
	}
	return args
}

// launchdFailure reads `launchctl print` output for a job that is not running
// and last exited non-zero (KeepAlive keeps respawning it). It returns "" for
// a healthy or never-run job.
func launchdFailure(print string) string {
	state, exit := "", ""
	for _, line := range strings.Split(print, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok && state == "" {
			state = v
		}
		if v, ok := strings.CutPrefix(line, "last exit code = "); ok && exit == "" {
			exit = v
		}
	}
	if state == "running" || exit == "" || exit == "0" || strings.HasPrefix(exit, "(never") {
		return ""
	}
	return "not running, last exit code " + exit
}
