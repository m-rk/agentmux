//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// findPaseoSupervisor looks for a loaded LaunchAgent in home that runs
// `paseo daemon`. A plist that is present but not loaded supervises nothing.
func findPaseoSupervisor(ctx context.Context, home string) (*paseoSupervisor, error) {
	plists, err := filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", "*.plist"))
	if err != nil {
		return nil, err
	}
	for _, path := range plists {
		out, err := exec.CommandContext(ctx, "plutil", "-convert", "json", "-o", "-", path).Output()
		if err != nil {
			continue // unreadable or not a plist; not ours to judge
		}
		var job struct {
			Label            string
			ProgramArguments []string
		}
		if json.Unmarshal(out, &job) != nil || job.Label == "" || !isPaseoDaemonCommand(job.ProgramArguments) {
			continue
		}
		state, err := exec.CommandContext(ctx, "launchctl", "print", launchdTarget(job.Label)).CombinedOutput()
		if err != nil {
			continue
		}
		return &paseoSupervisor{
			Kind:    "LaunchAgent",
			Name:    job.Label,
			Args:    job.ProgramArguments,
			Failing: launchdFailure(string(state)),
		}, nil
	}
	return nil, nil
}

// restartPaseoSupervisor restarts the job in place. Stopping the daemon with
// the paseo CLI instead would race launchd's KeepAlive respawn.
func restartPaseoSupervisor(ctx context.Context, s *paseoSupervisor) error {
	out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", launchdTarget(s.Name)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w: %s", s.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func launchdTarget(label string) string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
}
