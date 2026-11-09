package gateway

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// WarningBackend is implemented by a Backend that can report its own update
// problems alongside the installed versions. It is optional so a backend
// without it still serves versions unchanged.
type WarningBackend interface {
	VersionWarnings(ctx context.Context) []string
}

// VersionWarnings combines the lag check, which reads the ship gate, the
// versions record and the gate file's age under home, with the stuck-job
// check over jobs. Either input may be empty.
func VersionWarnings(home string, now time.Time, jobs []selfupdate.Job) []string {
	var out []string
	if home != "" {
		shipped, versions, _ := SelfUpdateFiles(home)
		gate, gerr := selfupdate.LoadCommits(shipped)
		installed, ierr := selfupdate.LoadCommits(versions)
		if info, err := os.Stat(shipped); gerr == nil && ierr == nil && err == nil {
			out = append(out, selfupdate.LagWarnings(now, gate, installed, info.ModTime())...)
		}
	}
	return append(out, selfupdate.StuckJobWarnings(jobs)...)
}

// ListLaunchdJobs reads `launchctl list` on macOS; elsewhere there is no
// launchd and nothing to report.
func ListLaunchdJobs() []selfupdate.Job {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("launchctl", "list").Output()
	if err != nil {
		return nil
	}
	return selfupdate.ParseLaunchctlList(string(out))
}
