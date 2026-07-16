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

// defaultAgentmuxStartInterval matches install-macos.sh's own default: how
// often the RunAtLoad LaunchAgent re-checks (idempotently) that the tmux
// session is still running.
const defaultAgentmuxStartInterval = 300

const defaultAgentmuxUpdateHour = 3
const defaultAgentmuxUpdateMinute = 0

// agentmuxPlistTemplate execs the pinned agentmux binary's own
// `session run --instance NAME` (idempotent, matching claude-code's own
// darwin plist) instead of backends/agentmux/rc-start.sh.
const agentmuxPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
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

const agentmuxUpdatePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
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

// createAgentmux is the native Go port of backends/agentmux/install-macos.sh
// (the "zero"/"opencode" + ollama backend), mirroring
// claudecode_darwin.go's createClaudeCode: never runs as root, needs no
// run_user, and installs two per-instance LaunchAgents instead of systemd
// units.
func createAgentmux(opts Options) (string, error) {
	if os.Geteuid() == 0 {
		return "", fmt.Errorf("must not create macOS instances as root/sudo; run as your normal user")
	}

	name := opts.InstanceName
	if name == "" {
		name = defaultAgentmuxInstance
	}
	if err := validateIdentifier("instance name", name); err != nil {
		return "", err
	}

	provider := opts.Provider
	if provider == "" {
		provider = "ollama"
	}
	if err := validateSupportedAgentProvider(opts.Agent, provider); err != nil {
		return "", err
	}

	sessionName := name
	if err := validateIdentifier("tmux session name", sessionName); err != nil {
		return "", err
	}

	model := opts.Model
	if model == "" {
		model = defaultOllamaModel
	}

	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolving current user: %w", err)
	}

	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}

	baseURL := providerBaseURL(provider)

	if err := checkAgentInstalledCurrentUser(opts.Agent); err != nil {
		return "", err
	}
	if provider == "ollama" {
		if err := checkOllama(); err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	label := "com.agentmux." + name
	updateLabel := label + ".update"

	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_AGENT", opts.Agent},
		{"AGENTMUX_PROVIDER", provider},
		{"AGENTMUX_MODEL", model},
		{"AGENTMUX_PROVIDER_BASE_URL", baseURL},
		{"AGENTMUX_PROVIDER_WAIT_SECONDS", defaultProviderWaitSecs},
		{"AGENTMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_TMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_WORKDIR", workdir},
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

	if err := installAgentmuxAgents(name, label, updateLabel, self); err != nil {
		return "", err
	}

	return fmt.Sprintf("Created instance %q (registry: %s). Reattach with: tmux -L agentmux-%s attach -t %s",
		name, regPath, name, sessionName), nil
}

// checkAgentInstalledCurrentUser mirrors install-macos.sh's
// `command -v "$AGENT"` check against the current user's own fixed-up
// PATH — no privilege drop needed, unlike Linux's checkAgentInstalled.
func checkAgentInstalledCurrentUser(agent string) error {
	if _, err := runas.CurrentUserLookPath(agent); err != nil {
		return fmt.Errorf("%s is not installed: %w", agent, err)
	}
	return nil
}

// checkOllama mirrors install-macos.sh's own check: ollama has no
// systemd-style service to query on macOS, so "is it up" means "can we
// actually reach it," the same thing RunAgentmux's own waitForProvider
// checks at start time.
func checkOllama() error {
	if runas.CurrentUserCommand("ollama", "list").Run() != nil {
		return fmt.Errorf("ollama is installed but not reachable; start it first (e.g. `brew services start ollama`) and retry")
	}
	return nil
}

func installAgentmuxAgents(name, label, updateLabel, binPath string) error {
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

	plist := fmt.Sprintf(agentmuxPlistTemplate, label, binPath, name, defaultAgentmuxStartInterval, logDir)
	updatePlist := fmt.Sprintf(agentmuxUpdatePlistTemplate, updateLabel, binPath, name, defaultAgentmuxUpdateHour, defaultAgentmuxUpdateMinute, logDir)

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
