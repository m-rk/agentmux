package provision

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// The amp LaunchAgents are byte-for-byte the same shape as the
// zero/opencode/kilo family's (agentmux_darwin.go): a
// RunAtLoad+StartInterval agent that idempotently ensures the tmux session
// is up, plus a daily StartCalendarInterval agent that updates the CLI.
// launchd has no ExecCondition equivalent, so — exactly as for the other two
// families on macOS — the "don't tick while the update runs" guard is left
// to the daemon's own per-instance behaviour rather than the service
// manager.
const ampPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%[1]s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%[2]s</string>
        <string>session</string>
        <string>run</string>
        <string>--instance</string>
        <string>%[3]s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>StartInterval</key>
    <integer>%[4]d</integer>
    <key>StandardOutPath</key>
    <string>%[5]s/%[3]s.log</string>
    <key>StandardErrorPath</key>
    <string>%[5]s/%[3]s.err.log</string>
</dict>
</plist>
`

const ampUpdatePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%[1]s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%[2]s</string>
        <string>session</string>
        <string>update</string>
        <string>--instance</string>
        <string>%[3]s</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>%[4]d</integer>
        <key>Minute</key>
        <integer>%[5]d</integer>
    </dict>
    <key>StandardOutPath</key>
    <string>%[6]s/%[3]s-update.log</string>
    <key>StandardErrorPath</key>
    <string>%[6]s/%[3]s-update.err.log</string>
</dict>
</plist>
`

// createAmp is the macOS counterpart of amp_linux.go's createAmp: never runs
// as root, needs no run_user, and installs two per-instance LaunchAgents
// instead of five systemd units. See the Linux version's doc comment for why
// this family exists separately from createAgentmux.
func createAmp(opts Options) (string, error) {
	if os.Geteuid() == 0 {
		return "", fmt.Errorf("must not create macOS instances as root/sudo; run as your normal user")
	}

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

	runnerID, err := AmpRunnerID(name)
	if err != nil {
		return "", err
	}

	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolving current user: %w", err)
	}

	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}
	hostName, err := resolveHostName(opts.HostName)
	if err != nil {
		return "", err
	}

	if err := checkAgentInstalledCurrentUser("amp"); err != nil {
		return "", err
	}
	if problem := ampAuthProblem(); problem != "" {
		return "", fmt.Errorf("%s; run 'amp login', then retry", problem)
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	label := "com.agentmux." + name
	updateLabel := label + ".update"

	_, alreadyExisted := existingAgentFor(name)

	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_AGENT", "amp"},
		{"AGENTMUX_AMP_RUNNER_ID", runnerID},
		{"AGENTMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_TMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_HOST_NAME", hostName},
		{"AGENTMUX_WORKDIR", workdir},
		{"AGENTMUX_RUN_USER", u.Username},
		{"AGENTMUX_SERVICE_NAME", label},
	})
	if err != nil {
		return "", err
	}

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	// installAmpAgents always boots the old LaunchAgent out before
	// bootstrapping the new one, so — as on the agentmux family's macOS path
	// — no separate stop-before-update step is needed here.
	if err := installAmpAgents(name, label, updateLabel, self); err != nil {
		return "", err
	}

	verb := "Created"
	if alreadyExisted {
		verb = "Updated"
	}
	return fmt.Sprintf("%s instance %q (registry: %s, amp runner-id: %s). Reattach with: tmux -L agentmux-%s attach -t %s",
		verb, name, regPath, runnerID, name, sessionName), nil
}

// ampAuthProblem checks login as the current user, since a macOS instance
// always runs as whoever invoked `agentmux new`; see ampAuthProblemVia for
// the shared parsing.
func ampAuthProblem() string {
	return ampAuthProblemVia(runas.CurrentUserCommand("amp", "usage"))
}

func installAmpAgents(name, label, updateLabel, binPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	logDir := filepath.Join(home, "Library", "Logs", "agentmux")
	for _, dir := range []string{launchAgentsDir, logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	plist := fmt.Sprintf(ampPlistTemplate, label, binPath, name, defaultAgentmuxStartInterval, logDir)
	updatePlist := fmt.Sprintf(ampUpdatePlistTemplate, updateLabel, binPath, name, defaultAgentmuxUpdateHour, defaultAgentmuxUpdateMinute, logDir)

	plistPath := filepath.Join(launchAgentsDir, label+".plist")
	updatePlistPath := filepath.Join(launchAgentsDir, updateLabel+".plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(updatePlistPath, []byte(updatePlist), 0o644); err != nil {
		return err
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, plistPath).Run()
	_ = exec.Command("launchctl", "bootout", domain, updatePlistPath).Run()
	if err := exec.Command("launchctl", "bootstrap", domain, plistPath).Run(); err != nil {
		return fmt.Errorf("bootstrapping %s: %w", label, err)
	}
	if err := exec.Command("launchctl", "bootstrap", domain, updatePlistPath).Run(); err != nil {
		return fmt.Errorf("bootstrapping %s: %w", updateLabel, err)
	}
	if err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).Run(); err != nil {
		return fmt.Errorf("starting %s: %w", label, err)
	}
	return nil
}
