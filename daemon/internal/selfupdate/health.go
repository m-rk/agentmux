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
// the job pinned to the old code signature (AMUX-125). It is one line, so a
// host with forty stuck jobs doesn't bury the rest of the output.
func StuckJobWarnings(jobs []Job) []string {
	var stuck []string
	for _, j := range jobs {
		if j.Running || j.Status == 0 {
			continue
		}
		if strings.HasPrefix(j.Label, "com.agentmux.") || strings.HasPrefix(j.Label, "com.m-rk.agentmux.") {
			stuck = append(stuck, fmt.Sprintf("%s (exit %d)", j.Label, j.Status))
		}
	}
	if len(stuck) == 0 {
		return nil
	}
	sort.Strings(stuck)
	return []string{fmt.Sprintf("%d launchd jobs failed to start and are not running: %s", len(stuck), strings.Join(stuck, ", "))}
}
