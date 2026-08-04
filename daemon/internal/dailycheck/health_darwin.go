package dailycheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func (platformProber) Probe(ctx context.Context, instance *pb.Instance) []HealthIssue {
	var issues []HealthIssue
	domain := "gui/" + strconv.Itoa(os.Getuid()) + "/"
	label := "com.agentmux." + instance.Name
	if output, err := launchdState(ctx, domain+label); err != nil {
		issues = append(issues, platformIssue(instance, "service-missing", "managed LaunchAgent is not loaded", err.Error()))
	} else if code, ok := launchdLastExitCode(output); ok && code != 0 {
		issues = append(issues, platformIssue(instance, "service-failed", "managed LaunchAgent last exited unsuccessfully", fmt.Sprintf("exit=%d", code)))
	}
	if output, err := launchdState(ctx, domain+label+".update"); err != nil {
		issues = append(issues, platformIssue(instance, "refresh-missing", "daily refresh LaunchAgent is not loaded", err.Error()))
	} else if launchdRunning(output) {
		issues = append(issues, platformIssue(instance, "refresh-running", "daily refresh is still running", "the update LaunchAgent has not exited yet"))
	} else if code, ok := launchdLastExitCode(output); ok && code != 0 {
		issues = append(issues, platformIssue(instance, "refresh-failed", "daily refresh failed", fmt.Sprintf("exit=%d", code)))
	}
	issues = append(issues, processIdentityIssue(ctx, instance)...)
	return issues
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
