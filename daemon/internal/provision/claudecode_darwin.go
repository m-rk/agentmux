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

const defaultClaudeCodeInstance = "claude-code"

// defaultStartInterval matches install-macos.sh's own default: how often
// the RunAtLoad LaunchAgent re-checks (idempotently) that the tmux session
// is still running.
const defaultStartInterval = 300

const defaultUpdateHour = 3
const defaultUpdateMinute = 0

// claudeCodePlistTemplate execs the pinned agentmux binary's own
// `session run --instance NAME` (idempotent: a no-op if the tmux session
// is already up) instead of backends/claude-code/rc-start.sh — state lives
// in the registry file this package writes, not launchd
// EnvironmentVariables, so the plist itself only needs to know how to
// invoke the binary. RunAtLoad+StartInterval (no KeepAlive) mirrors a
// systemd Type=oneshot unit: each run either starts the session or, if it's
// already up, exits immediately.
const claudeCodePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
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

const claudeCodeUpdatePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
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

// createClaudeCode is the native Go port of
// backends/claude-code/install-macos.sh: validate, resolve defaults, check
// login, pre-accept workspace trust, write the registry file, and
// install+load two per-instance LaunchAgents (a RunAtLoad+StartInterval
// unit that idempotently ensures the tmux session is running, and a daily
// StartCalendarInterval unit that updates Claude Code). Unlike Linux, this
// never runs as root and needs no run_user — a macOS instance always runs
// as the invoking user, matching install-macos.sh's own "must not run as
// root/sudo" check.
func createClaudeCode(opts Options) (string, error) {
	if os.Geteuid() == 0 {
		return "", fmt.Errorf("must not create macOS instances as root/sudo; run as your normal user")
	}

	name := opts.InstanceName
	if name == "" {
		name = defaultClaudeCodeInstance
	}
	if err := validateIdentifier("instance name", name); err != nil {
		return "", err
	}
	if opts.ResumeSessionID != "" {
		if err := validateIdentifier("resume session ID", opts.ResumeSessionID); err != nil {
			return "", err
		}
	}

	sessionName := name
	if name == defaultClaudeCodeInstance {
		sessionName = "agentmux"
	}
	if err := validateIdentifier("tmux session name", sessionName); err != nil {
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

	claudeJSON := claudeJSONPath(u.HomeDir)
	displayName := displayNameFor(u.Username, workdir)

	if !claudeLoggedIn() {
		return "", fmt.Errorf("Claude Code does not appear to be logged in; run 'claude' once to log in, then retry")
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	if err := preacceptWorkspaceTrust(claudeJSON, workdir, nil); err != nil {
		fmt.Printf("warning: could not pre-accept workspace trust: %v\n", err)
	}

	label := "com.agentmux." + name
	updateLabel := label + ".update"

	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_TMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_DISPLAY_NAME", displayName},
		{"AGENTMUX_SERVICE_NAME", label},
		{"AGENTMUX_WORKDIR", workdir},
		{"AGENTMUX_RESUME", opts.ResumeSessionID},
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

	if err := installClaudeCodeAgents(name, label, updateLabel, self); err != nil {
		return "", err
	}

	return fmt.Sprintf("Created instance %q (registry: %s). Reattach with: tmux -L agentmux-%s attach -t %s",
		name, regPath, name, sessionName), nil
}

// claudeLoggedIn checks login as the current user, since a macOS instance
// always runs as whoever invoked `agentmux new` — no privilege drop needed,
// unlike Linux's runas.Command(runUser, ...); see claudeLoggedInVia for the
// shared response parsing.
func claudeLoggedIn() bool {
	return claudeLoggedInVia(runas.CurrentUserCommand("claude", "auth", "status", "--json"))
}

func installClaudeCodeAgents(name, label, updateLabel, binPath string) error {
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

	plist := fmt.Sprintf(claudeCodePlistTemplate, label, binPath, name, defaultStartInterval, logDir)
	updatePlist := fmt.Sprintf(claudeCodeUpdatePlistTemplate, updateLabel, binPath, name, defaultUpdateHour, defaultUpdateMinute, logDir)

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
