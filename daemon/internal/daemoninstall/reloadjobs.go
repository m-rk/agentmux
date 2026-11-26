package daemoninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
)

// agentJobPrefixes are the label prefixes of every launchd job agentmux owns:
// the daemon, doctor, gc, gateway and self-update jobs (com.m-rk.agentmux.*)
// and each instance and instance-update job (com.agentmux.*).
var agentJobPrefixes = []string{"com.m-rk.agentmux.", "com.agentmux."}

// launchctl runs launchctl and returns its combined output. Replaced in tests.
var launchctl launchagent.Launchctl = launchagent.Run

// migrateAgentJobs moves every agentmux LaunchAgent in agentsDir that still
// starts the agentmux binary directly onto the /bin/sh launcher, once
// (AMUX-143). Such a job is tied to the code signature of the binary it was
// bootstrapped against: after a binary swap it fails to spawn with exit 78
// (AMUX-125), and re-registering it on every update made macOS post one
// "App Background Activity" notification per job per update. A job loaded
// through the launcher is not tied to the binary, so it is left alone: it
// runs the new binary the next time it starts.
//
// Each plist that runs agentmux directly is rewritten through the launcher.
// A loaded job is then booted out and back in if its plist was rewritten or
// launchd still runs agentmux for it directly. A job that is not loaded stays
// that way: `control stop` unloads a job on purpose, and loading it would
// start the instance again. The labels in skip are left alone, and the job
// running this process only has its file rewritten: booting it out would
// kill this process, so the self-update run re-registers it itself.
// A failure on one job does not stop the others; the returned error lists
// every failure.
func migrateAgentJobs(domain, agentsDir string, skip map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	self := os.Getenv("XPC_SERVICE_NAME") // the job running us
	var reloaded, failed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".plist") {
			continue
		}
		jobLabel := strings.TrimSuffix(name, ".plist")
		if !hasAgentJobPrefix(jobLabel) || skip[jobLabel] {
			continue
		}
		plist := filepath.Join(agentsDir, name)
		rewrote, err := wrapPlistFile(plist)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", jobLabel, err))
			continue
		}
		if jobLabel == self {
			continue
		}
		out, err := launchctl("print", domain+"/"+jobLabel)
		if err != nil {
			continue // not loaded: stopped on purpose
		}
		if !rewrote && !launchagent.PinnedToAgentmux(out) {
			continue // already through the launcher, or not an agentmux binary job
		}
		_, _ = launchctl("bootout", domain, plist)
		if out, err := launchctl("bootstrap", domain, plist); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v: %s", jobLabel, err, strings.TrimSpace(out)))
			continue
		}
		reloaded = append(reloaded, jobLabel)
	}
	sort.Strings(reloaded)
	if len(failed) > 0 {
		return reloaded, fmt.Errorf("moving launchd jobs onto the launcher: %s", strings.Join(failed, "; "))
	}
	return reloaded, nil
}

// refreshGatewayAgent puts an installed gateway on the new binary. A plist
// still starting agentmux directly is moved onto the launcher and
// re-registered, as is one launchd has not loaded; a loaded launcher job is
// only kickstarted, which restarts it on the new binary without a new
// registration.
func refreshGatewayAgent(domain, plist string) error {
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	rewrote, err := wrapPlistFile(plist)
	if err != nil {
		return err
	}
	out, err := launchctl("print", domain+"/"+launchagent.Label(plist))
	if err == nil && !rewrote && !launchagent.PinnedToAgentmux(out) {
		if out, err := launchctl("kickstart", "-k", domain+"/"+launchagent.Label(plist)); err != nil {
			return fmt.Errorf("launchctl kickstart %s: %w: %s", launchagent.Label(plist), err, strings.TrimSpace(out))
		}
		return nil
	}
	_, _ = launchctl("bootout", domain, plist)
	if out, err := launchctl("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", plist, err, strings.TrimSpace(out))
	}
	return nil
}

// wrapPlistFile rewrites plist through the launcher if it runs the agentmux
// binary directly, and reports whether it did.
func wrapPlistFile(plist string) (bool, error) {
	content, err := os.ReadFile(plist)
	if err != nil {
		return false, err
	}
	wrapped, ok := launchagent.Wrap(content)
	if !ok {
		return false, nil
	}
	return true, launchagent.WriteIfChanged(plist, wrapped)
}

func hasAgentJobPrefix(label string) bool {
	for _, p := range agentJobPrefixes {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return false
}
