package gateway

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
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
	jobs := selfupdate.ParseLaunchctlList(string(out))
	home, _ := os.UserHomeDir()
	for i, j := range jobs {
		if !j.Running && j.Status != 0 && home != "" {
			jobs[i].LastErr = jobLastErr(filepath.Join(home, "Library", "LaunchAgents"), j.Label)
		}
	}
	return jobs
}

// lastErrBytes is how much of a job's stderr log is read for its last line.
const lastErrBytes = 4096

// jobLastErr is the last line of a launchd job's stderr log, found through
// its plist's StandardErrorPath in agentsDir, or "" when either is missing.
// Only agentmux-owned labels are looked up.
func jobLastErr(agentsDir, label string) string {
	if !strings.HasPrefix(label, "com.agentmux.") && !strings.HasPrefix(label, "com.m-rk.agentmux.") {
		return ""
	}
	if strings.ContainsAny(label, "/\\") {
		return ""
	}
	plist, err := os.ReadFile(filepath.Join(agentsDir, label+".plist"))
	if err != nil {
		return ""
	}
	path := launchagent.StringValue(plist, "StandardErrorPath")
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > lastErrBytes {
		_, _ = f.Seek(st.Size()-lastErrBytes, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, lastErrBytes))
	return selfupdate.LastLine(b)
}
