package provision

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/m-rk/agentmux/daemon/internal/allowfiles"
	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// The codex units are the amp shape minus the updater: a persistent oneshot
// that keeps the placeholder tmux session up and a tick timer that re-runs
// `session run` when it is not. Codex runs headless per turn, so there is no
// long-lived agent process to update or supervise.
const codexUnitTemplate = `[Unit]
Description=Persistent agentmux codex instance %[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
User=%[2]s
ExecStart=%[3]s session run --instance %[1]s
ExecStop=%[3]s session stop --instance %[1]s
TimeoutStartSec=90
KillMode=process
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
`

const codexTickServiceTemplate = `[Unit]
Description=Periodic health check for agentmux codex instance %[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=%[2]s
ExecCondition=/bin/sh -c '! systemctl is-active --quiet agentmux-%[1]s.service'
ExecStart=%[3]s session run --instance %[1]s
TimeoutStartSec=90
`

const codexTickTimerTemplate = `[Unit]
Description=Periodic health check timer for agentmux codex instance %[1]s

[Timer]
OnUnitActiveSec=%[2]d
OnBootSec=%[2]d

[Install]
WantedBy=timers.target
`

// createCodex provisions a codex instance on Linux: registry entry plus the
// unit set above, after checking the CLI is installed and logged in.
func createCodex(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		name = defaultInstanceName(opts.Agent, opts.Workdir)
	}
	if err := validateIdentifier("instance name", name); err != nil {
		return "", err
	}
	if err := rejectUnsupportedCodexOptions(opts); err != nil {
		return "", err
	}
	sessionName := name
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
	if err := checkAgentInstalled("codex", runUser); err != nil {
		return "", err
	}
	if problem := codexLoginProblemVia(runas.Command(runUser, "codex", "login", "status")); problem != "" {
		return "", fmt.Errorf("%s; run 'codex login' as %s, then retry", problem, runUser)
	}
	if err := ensureWorkdirForUser(workdir, u); err != nil {
		return "", err
	}
	_, alreadyExisted := existingAgentFor(name)
	serviceName := "agentmux-" + name + ".service"
	tickServiceName := "agentmux-" + name + "-tick.service"
	tickTimerName := "agentmux-" + name + "-tick.timer"
	if alreadyExisted {
		if err := runSystemctl("stop", serviceName); err != nil {
			return "", fmt.Errorf("stopping %s before applying updated config: %w", serviceName, err)
		}
	}
	allowFiles, err := prepareAllowFiles(opts, workdir)
	if err != nil {
		return "", err
	}
	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_AGENT", "codex"},
		{"AGENTMUX_MODEL", opts.Model},
		{"AGENTMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_TMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_HOST_NAME", hostName},
		{"AGENTMUX_WORKDIR", workdir},
		{allowfiles.RegistryKey, allowFiles},
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
	units := map[string]string{
		serviceName:     fmt.Sprintf(codexUnitTemplate, name, runUser, self),
		tickServiceName: fmt.Sprintf(codexTickServiceTemplate, name, runUser, self),
		tickTimerName:   fmt.Sprintf(codexTickTimerTemplate, name, defaultTickIntervalSecs),
	}
	for unit, body := range units {
		if err := os.WriteFile("/etc/systemd/system/"+unit, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", serviceName}, {"enable", "--now", tickTimerName}} {
		if err := runSystemctl(args...); err != nil {
			return "", err
		}
	}
	verb := "Created"
	if alreadyExisted {
		verb = "Updated"
	}
	return fmt.Sprintf("%s instance %q (registry: %s). Runs are headless: use `agentmux sessions run`", verb, name, regPath), nil
}
