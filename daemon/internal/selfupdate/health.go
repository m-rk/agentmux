package selfupdate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LagThreshold is how long a host's installed commit may trail the shipped
// one before the hub is warned (AMUX-132).
const LagThreshold = 30 * time.Minute

// LagWarnings reports each repo whose installed commit differs from the
// shipped one once the gate has stood for longer than LagThreshold. The gate
// time is shipped.json's modification time: the updater never rewrites it, so
// it is when the hub last published. A repo that was never shipped, or is up
// to date, raises nothing.
func LagWarnings(now time.Time, shipped, installed map[string]string, shippedAt time.Time) []string {
	if shippedAt.IsZero() || now.Sub(shippedAt) <= LagThreshold {
		return nil
	}
	var out []string
	for _, repo := range Repos {
		want := shipped[repo]
		if want == "" || installed[repo] == want {
			continue
		}
		out = append(out, fmt.Sprintf("%s installed %s lags shipped %s for %d minutes: the host's updater is not installing it",
			repo, short(installed[repo]), short(want), int(now.Sub(shippedAt).Minutes())))
	}
	return out
}

func short(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Job is one row of `launchctl list`.
type Job struct {
	Label   string
	Running bool // a PID is shown
	Status  int  // last exit status, or the negated signal
	// LastErr is the last line of the job's own stderr log, when it could
	// be read: why the last run failed (AMUX-145).
	LastErr string
}

// maxLastErr bounds the stderr line quoted in a warning.
const maxLastErr = 200

// LastLine is the last non-empty line of a log, cut to a warning-sized
// length.
func LastLine(log []byte) string {
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxLastErr {
			line = string(r[:maxLastErr]) + "..."
		}
		return line
	}
	return ""
}

// ParseLaunchctlList parses `launchctl list` output: a header line, then
// "PID<TAB>Status<TAB>Label" per job, with "-" for no PID.
func ParseLaunchctlList(out string) []Job {
	var jobs []Job
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] == "PID" {
			continue
		}
		status, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		jobs = append(jobs, Job{Label: f[2], Running: f[0] != "-", Status: status})
	}
	return jobs
}

// StuckJobWarnings names agentmux-owned launchd jobs that are not running and
// last exited non-zero, such as exit 78 (EX_CONFIG) after a binary swap left
// the job pinned to the old code signature (AMUX-125), or a scheduled job
// whose last run failed. It is one line, so a host with forty stuck jobs
// doesn't bury the rest of the output; each job's last stderr line, when
// known, says why.
func StuckJobWarnings(jobs []Job) []string {
	var stuck []string
	for _, j := range jobs {
		if j.Running || j.Status == 0 {
			continue
		}
		if strings.HasPrefix(j.Label, "com.agentmux.") || strings.HasPrefix(j.Label, "com.m-rk.agentmux.") {
			if j.LastErr != "" {
				stuck = append(stuck, fmt.Sprintf("%s (exit %d: %s)", j.Label, j.Status, j.LastErr))
			} else {
				stuck = append(stuck, fmt.Sprintf("%s (exit %d)", j.Label, j.Status))
			}
		}
	}
	if len(stuck) == 0 {
		return nil
	}
	sort.Strings(stuck)
	return []string{fmt.Sprintf("%d launchd jobs last exited non-zero and are not running: %s", len(stuck), strings.Join(stuck, ", "))}
}
