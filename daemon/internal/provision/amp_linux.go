package provision

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// The amp unit templates deliberately mirror the zero/opencode/kilo family
// (agentmux_linux.go) rather than claude-code's: a persistent
// Type=oneshot/RemainAfterExit=yes service, a nightly update timer, and a
// short-interval tick timer carrying the same ExecCondition guard. amp has
// no --resume/compact concept, so none of claude-code's session-transcript
// machinery applies.
//
// TimeoutStartSec=90 matches the agentmux family rather than claude-code's
// 30: `amp --no-tui` has to reach ampcode.com and register the runner before
// it is useful, and the tick's own start deadline should not be tighter than
// the cold start it supervises.
//
// No After=ollama.service here (unlike agentmuxUnitTemplate): amp talks to
// ampcode.com, never to a local model server.
const ampUnitTemplate = `[Unit]
Description=Persistent agentmux amp runner %[1]s (runner-id %[2]s)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
User=%[3]s
ExecStart=%[4]s session run --instance %[1]s
ExecStop=%[4]s session stop --instance %[1]s
TimeoutStartSec=90
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
`

const ampUpdateUnitTemplate = `[Unit]
Description=Update the Amp CLI and restart agentmux instance %[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%[2]s session update --instance %[1]s
TimeoutStartSec=1200
`

const ampTimerTemplate = `[Unit]
Description=Periodic maintenance for agentmux amp instance %[1]s

[Timer]
OnCalendar=%[2]s
Persistent=true
RandomizedDelaySec=120

[Install]
WantedBy=timers.target
`

// ExecCondition skips this run (a clean no-op, not a failure) while the
// nightly update is active for the same instance — the same guard the other
// two families carry; see claudeCodeTickServiceTemplate's doc comment in
// claudecode_linux.go for the reasoning.
const ampTickServiceTemplate = `[Unit]
Description=Periodic health check for agentmux amp instance %[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=%[2]s
ExecCondition=/bin/sh -c '! systemctl is-active --quiet %[4]s'
ExecStart=%[3]s session run --instance %[1]s
TimeoutStartSec=90
`

const ampTickTimerTemplate = `[Unit]
Description=Periodic health check timer for agentmux amp instance %[1]s

[Timer]
OnUnitActiveSec=%[2]d
OnBootSec=%[2]d

[Install]
WantedBy=timers.target
`

// createAmp provisions an amp instance on Linux. It follows
// createAgentmux's structure (unit set, stop-before-rewrite on
// re-provision, registry fields) minus everything provider-related, and
// borrows one thing from createClaudeCode instead: a real login preflight,
// because an unauthenticated `amp --no-tui` blocks on an interactive prompt
// forever rather than exiting — see ampAuthProblemVia.
func createAmp(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		name = defaultInstanceName(opts.Agent, opts.Workdir)
	}
	if err := validateIdentifier("instance name", name); err != nil {
		return "", err
	}
	if err := rejectUnsupportedAmpOptions(opts); err != nil {
		return "", err
	}

	sessionName := name
	if err := validateIdentifier("tmux session name", sessionName); err != nil {
		return "", err
	}

	// The runner ID is the instance name with any trailing -<agent> suffix
	// stripped (so the runner on ampcode.com is the clean project name, not
	// the agentmux-suffixed instance name), sanitized into a valid hostname.
	// Computed (and stored) once here rather than re-derived on every
	// session run so a future change to the sanitizer can never silently
	// re-register a long-lived instance under a different runner ID.
	runnerID, err := AmpRunnerID(strings.TrimSuffix(name, "-"+opts.Agent))
	if err != nil {
		return "", err
	}

	// Explicit extra --dir entries must be absolute: a relative path would
	// resolve against the daemon's own working directory, never the
	// operator's intent. Callers expand ~ themselves (an unquoted ~
	// in the shell already does).
	serveDirs := AmpSplitDirs(opts.AmpDirs)
	for _, d := range serveDirs {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("amp dir %q is not absolute — pass absolute paths (expand ~ first)", d)
		}
	}
	managedUpdate, err := ampManagedUpdate(opts.AmpUpdate)
	if err != nil {
		return "", err
	}

	// Captured before writeRegistry below overwrites the file — same reason
	// as createAgentmux: it decides whether there's a live process to stop
	// first.
	_, alreadyExisted := existingAgentFor(name)

	runUser := opts.RunUser
	if runUser == "" {
		return "", fmt.Errorf("run_user is required")
	}
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", fmt.Errorf("looking up user %q: %w", runUser, err)
	}

	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}
	hostName, err := resolveHostName(opts.HostName)
	if err != nil {
		return "", err
	}

	if err := checkAgentInstalled("amp", runUser); err != nil {
		return "", err
	}
	if problem := ampInstallPackageProblem(runUser); problem != "" {
		return "", fmt.Errorf("%s", problem)
	}
	if problem := ampAuthProblem(runUser); problem != "" {
		return "", fmt.Errorf("%s; run 'amp login' as %s, then retry", problem, runUser)
	}

	if err := ensureWorkdirForUser(workdir, u); err != nil {
		return "", err
	}

	serviceName := "agentmux-" + name + ".service"
	updateServiceName := "agentmux-" + name + "-update.service"
	timerName := "agentmux-" + name + "-update.timer"
	tickServiceName := "agentmux-" + name + "-tick.service"
	tickTimerName := "agentmux-" + name + "-tick.timer"

	if alreadyExisted {
		if err := runSystemctl("stop", serviceName); err != nil {
			return "", fmt.Errorf("stopping %s before applying updated config: %w", serviceName, err)
		}
	}

	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_AGENT", "amp"},
		{"AGENTMUX_AMP_RUNNER_ID", runnerID},
		{"AGENTMUX_AMP_DIRS", strings.Join(serveDirs, ",")},
		{"AGENTMUX_AMP_DISCOVER_DIRS", discoverFlag(opts.AmpDiscoverDirs)},
		{"AGENTMUX_AMP_UPDATE", opts.AmpUpdate},
		{"AGENTMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_TMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_HOST_NAME", hostName},
		{"AGENTMUX_WORKDIR", workdir},
		{"AGENTMUX_RUN_USER", runUser},
		{"AGENTMUX_SERVICE_NAME", serviceName},
	})
	if err != nil {
		return "", err
	}
	if err := chownRegistryForUser(name, u); err != nil {
		return "", err
	}

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	if err := installAmpUnits(name, runnerID, runUser, self, serviceName, updateServiceName, timerName, tickServiceName, tickTimerName, managedUpdate); err != nil {
		return "", err
	}

	verb := "Created"
	if alreadyExisted {
		verb = "Updated"
	}
	msg := fmt.Sprintf("%s instance %q (registry: %s, amp runner-id: %s). Reattach with: sudo -u %s tmux -L agentmux-%s attach -t %s",
		verb, name, regPath, runnerID, runUser, name, sessionName)
	if !managedUpdate {
		msg += " (agentmux updater off; runner self-updates)"
	}
	return msg, nil
}

// ampAuthProblem checks login by dropping privileges to runUser, since this
// provisioner runs as root; see ampAuthProblemVia for the shared parsing.
func ampAuthProblem(runUser string) string {
	return ampAuthProblemVia(runas.Command(runUser, "amp", "usage"))
}

// ampInstallPackageProblem checks the npm-global install by dropping
// privileges to runUser, since this provisioner runs as root; see
// ampInstallPackageProblemVia for the shared check.
func ampInstallPackageProblem(runUser string) string {
	return ampInstallPackageProblemVia(runas.Command(runUser, "npm", "ls", "-g", "@sourcegraph/amp", "--depth=0"))
}

func installAmpUnits(name, runnerID, runUser, binPath, serviceName, updateServiceName, timerName, tickServiceName, tickTimerName string, managedUpdate bool) error {
	unit := fmt.Sprintf(ampUnitTemplate, name, runnerID, runUser, binPath)
	timer := fmt.Sprintf(ampTimerTemplate, name, defaultOnCalendar)
	tickService := fmt.Sprintf(ampTickServiceTemplate, name, runUser, binPath, updateServiceName)
	tickTimer := fmt.Sprintf(ampTickTimerTemplate, name, defaultTickIntervalSecs)

	if err := os.WriteFile("/etc/systemd/system/"+serviceName, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+tickServiceName, []byte(tickService), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+tickTimerName, []byte(tickTimer), 0o644); err != nil {
		return err
	}
	if managedUpdate {
		updateUnit := fmt.Sprintf(ampUpdateUnitTemplate, name, binPath)
		if err := os.WriteFile("/etc/systemd/system/"+updateServiceName, []byte(updateUnit), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile("/etc/systemd/system/"+timerName, []byte(timer), 0o644); err != nil {
			return err
		}
	} else {
		// The runner self-updates; an agentmux-driven update would fight
		// its own updater. Stop and remove a stale update unit/timer left
		// by an earlier provisioning that had updates on, so the nightly
		// `session update` stops firing. Missing units are fine (fresh
		// instance that never had updates on).
		_ = runSystemctl("disable", "--now", updateServiceName)
		_ = runSystemctl("disable", "--now", timerName)
		for _, path := range []string{"/etc/systemd/system/" + updateServiceName, "/etc/systemd/system/" + timerName} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing stale %s: %w", path, err)
			}
		}
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", serviceName); err != nil {
		return err
	}
	if managedUpdate {
		if err := runSystemctl("enable", "--now", timerName); err != nil {
			return err
		}
	}
	return runSystemctl("enable", "--now", tickTimerName)
}
