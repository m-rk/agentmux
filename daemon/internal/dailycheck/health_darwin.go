package dailycheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

// exConfigExitCode is sysexits.h's EX_CONFIG (78): launchd's own synthetic
// posix_spawn failure code, reported when a job's launchd-level
// registration is broken (not something the job's program itself
// returned — no agentmux/claude-code binary ever exits 78, and running the
// exact same command manually, outside launchd, has repeatedly worked fine
// when this hits). `launchctl kickstart -k` alone does not clear it; only a
// full bootout+bootstrap reload from the plist does. Because this failure
// mode is purely a launchd bookkeeping problem — it never touches session
// content — it's safe to auto-repair deterministically here, unlike a real
// refresh failure, which the analyzer deliberately never restarts for.
const exConfigExitCode = 78

func (platformProber) Probe(ctx context.Context, instance *pb.Instance) []HealthIssue {
	var issues []HealthIssue
	domain := "gui/" + strconv.Itoa(os.Getuid()) + "/"
	label := "com.agentmux." + instance.Name
	if output, err := launchdState(ctx, domain+label); err != nil {
		issues = append(issues, platformIssue(instance, "service-missing", "managed LaunchAgent is not loaded", err.Error()))
	} else if code, ok := launchdLastExitCode(output); ok && code != 0 {
		issues = append(issues, platformIssue(instance, "service-failed", "managed LaunchAgent last exited unsuccessfully", fmt.Sprintf("exit=%d", code)))
	}
	updateLabel := label + ".update"
	if output, err := launchdState(ctx, domain+updateLabel); err != nil {
		issues = append(issues, platformIssue(instance, "refresh-missing", "daily refresh LaunchAgent is not loaded", err.Error()))
	} else if launchdRunning(output) {
		issues = append(issues, platformIssue(instance, "refresh-running", "daily refresh is still running", "the update LaunchAgent has not exited yet"))
	} else if code, ok := launchdLastExitCode(output); ok && code != 0 {
		detail := fmt.Sprintf("exit=%d", code)
		if code == exConfigExitCode {
			// Reload only (no kickstart): this repairs the wedged
			// registration so the job's next legitimate scheduled tick
			// succeeds, without forcing an unscheduled compact+restart of
			// the live session the way kickstarting it now would.
			if repairErr := reloadLaunchdJob(ctx, updateLabel); repairErr != nil {
				detail += fmt.Sprintf(" (auto-repair failed: %s)", repairErr)
			} else {
				detail += " (auto-repaired: launchd registration reloaded; will confirm at its next scheduled run)"
			}
		}
		issues = append(issues, platformIssue(instance, "refresh-failed", "daily refresh failed", detail))
	}
	issues = append(issues, processIdentityIssue(ctx, instance)...)
	return issues
}

// reloadLaunchdJob clears a wedged launchd-level job registration via
// bootout (ignored if not currently loaded) followed by bootstrap from the
// job's own plist. It never kicks the job to run immediately.
func reloadLaunchdJob(ctx context.Context, label string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain, plist).Run()
	if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", plist, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func launchdRunning(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "state = running" {
			return true
		}
	}
	return false
}

func launchdState(ctx context.Context, target string) (string, error) {
	out, err := exec.CommandContext(ctx, "launchctl", "print", target).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("launchctl print %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func launchdLastExitCode(output string) (int, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, "last exit code = ")
		if !ok {
			continue
		}
		code, err := strconv.Atoi(strings.TrimSpace(value))
		return code, err == nil
	}
	return 0, false
}

func platformIssue(instance *pb.Instance, code, summary, detail string) HealthIssue {
	return HealthIssue{Instance: instance.Name, Code: code, Summary: summary, Detail: detail}
}
