package dailycheck

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func (platformProber) Probe(ctx context.Context, instance *pb.Instance) []HealthIssue {
	var issues []HealthIssue
	service := "agentmux-" + instance.Name + ".service"
	if props, err := systemdProperties(ctx, service); err != nil {
		issues = append(issues, platformIssue(instance, "service-unreadable", "managed service state could not be inspected", err.Error()))
	} else {
		if props["LoadState"] != "loaded" {
			issues = append(issues, platformIssue(instance, "service-missing", "managed service unit is not loaded", service))
		} else if props["ActiveState"] == "failed" || failedResult(props["Result"], props["ExecMainStatus"]) {
			issues = append(issues, platformIssue(instance, "service-failed", "managed service is failed", describeFailure(ctx, service, props)))
		} else if props["ActiveState"] != "active" {
			issues = append(issues, platformIssue(instance, "service-inactive", "managed service is not active", describeProperties(props)))
		}
	}

	updateService := "agentmux-" + instance.Name + "-update.service"
	if props, err := systemdProperties(ctx, updateService); err != nil {
		issues = append(issues, platformIssue(instance, "refresh-unreadable", "daily refresh state could not be inspected", err.Error()))
	} else if props["LoadState"] != "loaded" {
		issues = append(issues, platformIssue(instance, "refresh-missing", "daily refresh unit is not loaded", updateService))
	} else if props["ActiveState"] == "active" || props["ActiveState"] == "activating" {
		issues = append(issues, platformIssue(instance, "refresh-running", "daily refresh is still running", describeProperties(props)))
	} else if failedResult(props["Result"], props["ExecMainStatus"]) {
		issues = append(issues, platformIssue(instance, "refresh-failed", "daily refresh failed", describeFailure(ctx, updateService, props)))
	}

	issues = append(issues, processIdentityIssue(ctx, instance)...)
	return issues
}

func systemdProperties(ctx context.Context, unit string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit,
		"--property=LoadState,ActiveState,SubState,Result,ExecMainStatus,NRestarts").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	return props, nil
}

func failedResult(result, status string) bool {
	if result != "" && result != "success" {
		return true
	}
	code, err := strconv.Atoi(status)
	return err == nil && code != 0
}

func describeProperties(props map[string]string) string {
	return fmt.Sprintf("active=%s sub=%s result=%s exit=%s restarts=%s",
		props["ActiveState"], props["SubState"], props["Result"], props["ExecMainStatus"], props["NRestarts"])
}

// describeFailure is describeProperties plus the unit's own last log line.
// Confirmed live: systemd's ActiveState/Result/ExecMainStatus alone gave no
// hint that an update had failed with "fork/exec ...: exec format error" —
// finding that required a manual journalctl dig doctor should have
// surfaced itself.
func describeFailure(ctx context.Context, unit string, props map[string]string) string {
	summary := describeProperties(props)
	line, err := lastAppLogLine(ctx, unit)
	if err != nil || line == "" {
		return summary
	}
	return summary + " last: " + line
}

// lastAppLogLine returns the unit's own last log line, filtered to
// _COMM=agentmux so it's the app's own message rather than systemd's
// surrounding "Main process exited"/"Failed with result" bookkeeping,
// which journalctl -n 1 without the filter would return instead. A package
// var so tests can swap it without a real systemd unit to log against.
var lastAppLogLine = func(ctx context.Context, unit string) (string, error) {
	out, err := exec.CommandContext(ctx, "journalctl", "-u", unit, "_COMM=agentmux", "-n", "1", "-o", "cat", "--no-pager").Output()
	return strings.TrimSpace(string(out)), err
}

func platformIssue(instance *pb.Instance, code, summary, detail string) HealthIssue {
	return HealthIssue{Instance: instance.Name, Code: code, Summary: summary, Detail: detail}
}
