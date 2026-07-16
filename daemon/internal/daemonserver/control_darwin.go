package daemonserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// applyControl implements start/stop/restart for macOS instances, whose
// "service" (inst.ServiceName) is a per-instance LaunchAgent label (e.g.
// "com.agentmux.<name>", set by provision/claudecode_darwin.go) plus the
// tmux session it supervises — there's no systemd-style "restart the
// service" primitive to shell out to, so each action is implemented
// directly:
//   - stop: kill the tmux session AND unload both the main LaunchAgent and
//     its ".update" sibling, so neither RunAtLoad/StartInterval polling nor
//     the daily update job's own ensure-running step resurrects the
//     session behind the caller's back.
//   - start: reload both LaunchAgents (bootout-then-bootstrap, so it's safe
//     to call whether or not either is currently loaded) and kick the main
//     one immediately rather than waiting for the next tick.
//   - restart: both LaunchAgents stay loaded throughout; just stop then
//     start the tmux session directly via the session package, the same
//     way session update's own restart-after-update does.
func applyControl(ctx context.Context, inst discovery.Instance, action string) (bool, string) {
	updateLabel := inst.ServiceName + ".update"
	switch action {
	case "stop":
		_ = session.Stop(inst.Name)
		errMain := unloadAgent(ctx, inst.ServiceName)
		errUpdate := unloadAgent(ctx, updateLabel)
		if errMain != nil || errUpdate != nil {
			return false, fmt.Sprintf("stopped session but failed to unload %s/%s: %v / %v", inst.ServiceName, updateLabel, errMain, errUpdate)
		}
		return true, fmt.Sprintf("stopped %s", inst.Name)
	case "start":
		if err := loadAgent(ctx, updateLabel, false); err != nil {
			return false, fmt.Sprintf("starting %s: %v", updateLabel, err)
		}
		if err := loadAgent(ctx, inst.ServiceName, true); err != nil {
			return false, fmt.Sprintf("starting %s: %v", inst.ServiceName, err)
		}
		return true, fmt.Sprintf("started %s", inst.Name)
	case "restart":
		_ = session.Stop(inst.Name)
		if err := session.Run(inst.Name); err != nil {
			return false, fmt.Sprintf("restarting %s: %v", inst.Name, err)
		}
		return true, fmt.Sprintf("restarted %s", inst.Name)
	default:
		return false, "unknown action"
	}
}

func plistPathForLabel(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func unloadAgent(ctx context.Context, label string) error {
	plist, err := plistPathForLabel(label)
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	return exec.CommandContext(ctx, "launchctl", "bootout", domain, plist).Run()
}

// loadAgent (re)loads the LaunchAgent labeled label. kickstart, if true,
// also forces it to run immediately rather than waiting for its next
// RunAtLoad/StartInterval/StartCalendarInterval tick — used for the main
// label (so "start" doesn't just leave the tmux session down until the next
// poll) but not the ".update" label (there's nothing to run early: it's
// meant to fire at its scheduled time, not the moment "start" is clicked).
func loadAgent(ctx context.Context, label string, kickstart bool) error {
	plist, err := plistPathForLabel(label)
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain, plist).Run()
	if err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, plist).Run(); err != nil {
		return err
	}
	if !kickstart {
		return nil
	}
	return exec.CommandContext(ctx, "launchctl", "kickstart", "-k", domain+"/"+label).Run()
}
