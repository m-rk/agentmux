package dailycheck

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// HealthIssue is a deterministic doctor finding. Claude receives these facts
// only when at least one issue exists; healthy sessions never trigger a model
// call.
type HealthIssue struct {
	Instance string `json:"instance"`
	Code     string `json:"code"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
}

type PlatformProber interface {
	Probe(context.Context, *pb.Instance) []HealthIssue
}

type platformProber struct{}

func NewPlatformProber() PlatformProber { return platformProber{} }

func collectHealth(ctx context.Context, client Client, opts Options) ([]Snapshot, []HealthIssue, error) {
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing instances: %w", err)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })

	snapshots := make([]Snapshot, 0, len(instances))
	var issues []HealthIssue
	for _, instance := range instances {
		snapshot := Snapshot{
			Name:         instance.Name,
			Agent:        instance.Agent,
			Provider:     instance.Provider,
			Model:        instance.Model,
			Status:       statusName(instance.Status),
			LastActivity: instance.LastActivityUnix,
		}
		if instance.Status != pb.Status_STATUS_DEAD {
			pane, paneErr := client.ViewPane(ctx, &pb.ViewPaneRequest{
				Instance:        instance.Name,
				ScrollbackLines: opts.PaneLines,
			})
			if paneErr != nil {
				snapshot.CaptureError = paneErr.Error()
			} else {
				snapshot.Pane = tailBytes(pane.Content, opts.MaxPaneBytes)
			}
		}

		snapshot.Issues = append(snapshot.Issues, coreHealthIssues(instance, snapshot)...)
		if opts.PlatformProber != nil {
			snapshot.Issues = append(snapshot.Issues, opts.PlatformProber.Probe(ctx, instance)...)
		}
		issues = append(issues, snapshot.Issues...)
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, issues, nil
}

func coreHealthIssues(instance *pb.Instance, snapshot Snapshot) []HealthIssue {
	issue := func(code, summary, detail string) HealthIssue {
		return HealthIssue{Instance: instance.Name, Code: code, Summary: summary, Detail: detail}
	}
	var issues []HealthIssue
	switch instance.Status {
	case pb.Status_STATUS_DEAD:
		return []HealthIssue{issue("session-dead", "session is not running", "agentmux found no managed tmux session")}
	case pb.Status_STATUS_UNKNOWN:
		issues = append(issues, issue("session-unknown", "session state is unknown", "agentmux could not determine tmux liveness"))
	}
	if instance.TmuxSession == "" {
		issues = append(issues, issue("identity-missing", "tmux session identity is missing", "the daemon returned an empty tmux session name"))
	}
	if instance.Pid <= 0 {
		issues = append(issues, issue("process-missing", "session process identity is missing", "the daemon returned no pane PID"))
	}
	if snapshot.CaptureError != "" {
		issues = append(issues, issue("pane-unreadable", "session pane could not be inspected", snapshot.CaptureError))
		return issues
	}
	if strings.TrimSpace(snapshot.Pane) == "" {
		issues = append(issues, issue("pane-empty", "session pane is empty", "the process is present but has not rendered an interactive pane"))
		return issues
	}

	switch instance.Agent {
	case "claude-code":
		switch {
		case session.ClaudePaneRemoteMenuOpen(snapshot.Pane):
			issues = append(issues, issue("stuck-menu", "Claude Remote Control menu is covering the session", "the visible menu can be dismissed safely with Escape"))
		case !session.ClaudePaneRemoteConnected(snapshot.Pane):
			issues = append(issues, issue("remote-disconnected", "Claude Remote Control is not connected", "the /rc indicator is absent from the footer"))
		}
	case "kilo":
		if !session.KiloPaneReady(snapshot.Pane) {
			issues = append(issues, issue("backend-not-ready", "Kilo has not reached its interactive UI", "the stable command footer is absent"))
		} else if !session.KiloPaneRemoteConnected(snapshot.Pane) {
			issues = append(issues, issue("remote-disconnected", "Kilo remote relay is not connected", "the Remote footer indicator is absent"))
		}
	}
	return issues
}

func processIdentityIssue(ctx context.Context, instance *pb.Instance) []HealthIssue {
	if instance.Pid <= 0 || instance.Status == pb.Status_STATUS_DEAD {
		return nil
	}
	commands, err := processTreeCommands(ctx, instance.Pid)
	if err != nil {
		return []HealthIssue{platformIssue(instance, "process-unreadable", "session process could not be inspected", err.Error())}
	}
	expected := instance.Agent
	if expected == "claude-code" {
		expected = "claude"
	}
	for _, command := range commands {
		if strings.Contains(strings.ToLower(command), strings.ToLower(expected)) {
			return nil
		}
	}
	return []HealthIssue{platformIssue(instance, "process-mismatch", "session process tree does not contain its configured agent", "expected a running "+expected+" process beneath the tmux pane")}
}

// processTreeCommands accepts the tmux pane PID as either the agent itself or
// a shell parent. tmux implementations/platforms differ on whether the pane's
// launch shell execs the final agent command, so checking only pane_pid would
// report healthy shell+agent trees as mismatches.
func processTreeCommands(ctx context.Context, rootPID int64) ([]string, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,command=").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	type process struct {
		pid, ppid int64
		command   string
	}
	var processes []process
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.ParseInt(fields[0], 10, 64)
		ppid, ppidErr := strconv.ParseInt(fields[1], 10, 64)
		if pidErr != nil || ppidErr != nil {
			continue
		}
		processes = append(processes, process{pid: pid, ppid: ppid, command: strings.Join(fields[2:], " ")})
	}
	selected := map[int64]bool{rootPID: true}
	var commands []string
	for changed := true; changed; {
		changed = false
		for _, process := range processes {
			if selected[process.pid] {
				continue
			}
			if selected[process.ppid] {
				selected[process.pid] = true
				changed = true
			}
		}
	}
	for _, process := range processes {
		if selected[process.pid] {
			commands = append(commands, process.command)
		}
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("pane PID %d is not present", rootPID)
	}
	return commands, nil
}

func issueFingerprint(issues []HealthIssue, problems []string) string {
	parts := make([]string, 0, len(issues)+len(problems))
	for _, issue := range issues {
		parts = append(parts, issue.Instance+":"+issue.Code)
	}
	for _, problem := range problems {
		parts = append(parts, "doctor:"+cleanOneLine(problem))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
