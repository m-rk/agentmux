package daemoninstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// agentJobPrefixes are the label prefixes of every launchd job agentmux owns:
// the daemon, doctor, gc, gateway and self-update jobs (com.m-rk.agentmux.*)
// and each instance and instance-update job (com.agentmux.*).
var agentJobPrefixes = []string{"com.m-rk.agentmux.", "com.agentmux."}

// launchctl runs launchctl and returns its combined output. Replaced in tests.
var launchctl = func(args ...string) (string, error) {
	return runCmdOutput("launchctl", args...)
}

// reloadLoadedAgentJobs re-registers (bootout then bootstrap) every agentmux
// LaunchAgent in agentsDir that is currently loaded, except the labels in
// skip and the job this process is running under. launchd pins a job to the
// code signature of the binary it was bootstrapped against, so after the
// binary is replaced every job not re-registered fails to spawn with exit 78
// and nothing logs it (AMUX-125). Install used to re-register only the
// daemon, doctor, gc and gateway jobs; the ~40 instance and instance-update
// jobs and the self-update job were left pinned to the old binary.
//
// A job that is not loaded stays that way: `control stop` unloads a job on
// purpose, and re-registering it would start the instance again. A failure on
// one job does not stop the others; the returned error lists every failure.
func reloadLoadedAgentJobs(domain, agentsDir string, skip map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	self := os.Getenv("XPC_SERVICE_NAME") // the job running us; booting it out would kill this process
	var reloaded, failed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".plist") {
			continue
		}
		jobLabel := strings.TrimSuffix(name, ".plist")
		if !hasAgentJobPrefix(jobLabel) || skip[jobLabel] || jobLabel == self {
			continue
		}
		if _, err := launchctl("print", domain+"/"+jobLabel); err != nil {
			continue // not loaded: stopped on purpose
		}
		plist := filepath.Join(agentsDir, name)
		_, _ = launchctl("bootout", domain, plist)
		if out, err := launchctl("bootstrap", domain, plist); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v: %s", jobLabel, err, strings.TrimSpace(out)))
			continue
		}
		reloaded = append(reloaded, jobLabel)
	}
	sort.Strings(reloaded)
	if len(failed) > 0 {
		return reloaded, fmt.Errorf("re-registering launchd jobs after the binary swap: %s", strings.Join(failed, "; "))
	}
	return reloaded, nil
}

func hasAgentJobPrefix(label string) bool {
	for _, p := range agentJobPrefixes {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return false
}

func runCmdOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
