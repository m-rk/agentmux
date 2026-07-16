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

const (
	defaultAgentmuxInstance = "agentmux"
	defaultOllamaModel      = "gpt-oss:20b-cloud"
	defaultProviderWaitSecs = "60"
)

const agentmuxUnitTemplate = `[Unit]
Description=Persistent agentmux instance %[1]s (%[2]s + %[3]s)
After=network-online.target ollama.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
User=%[4]s
ExecStart=%[5]s session run --instance %[1]s
ExecStop=%[5]s session stop --instance %[1]s
TimeoutStartSec=30

[Install]
WantedBy=multi-user.target
`

const agentmuxUpdateUnitTemplate = `[Unit]
Description=Maintain agentmux instance %[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%[2]s session update --instance %[1]s
`

const agentmuxTimerTemplate = `[Unit]
Description=Periodic maintenance for agentmux instance %[1]s

[Timer]
OnCalendar=%[2]s
Persistent=true
RandomizedDelaySec=120

[Install]
WantedBy=timers.target
`

// createAgentmux is the native Go port of backends/agentmux/install.sh's
// Linux path (the "zero"/"opencode" + ollama backend), mirroring
// createClaudeCode's structure.
func createAgentmux(opts Options) (string, error) {
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

	runUser := opts.RunUser
	if runUser == "" {
		return "", fmt.Errorf("run_user is required")
	}
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", fmt.Errorf("looking up user %q: %w", runUser, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}

	baseURL := providerBaseURL(provider)

	if err := checkAgentInstalled(opts.Agent, runUser); err != nil {
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
	if err := os.Chown(workdir, uid, gid); err != nil {
		return "", fmt.Errorf("chown workdir: %w", err)
	}

	serviceName := "agentmux-" + name + ".service"
	updateServiceName := "agentmux-" + name + "-update.service"
	timerName := "agentmux-" + name + "-update.timer"

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
		{"AGENTMUX_RUN_USER", runUser},
		{"AGENTMUX_SERVICE_NAME", serviceName},
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

	if err := installAgentmuxUnits(name, opts.Agent, provider, runUser, self, serviceName, updateServiceName, timerName); err != nil {
		return "", err
	}

	return fmt.Sprintf("Created instance %q (registry: %s). Reattach with: sudo -u %s tmux -L agentmux-%s attach -t %s",
		name, regPath, runUser, name, sessionName), nil
}

func validateSupportedAgentProvider(agent, provider string) error {
	switch agent + ":" + provider {
	case "zero:ollama", "opencode:ollama":
		return nil
	default:
		return fmt.Errorf("unsupported agent/provider combination: %s/%s", agent, provider)
	}
}

func providerBaseURL(provider string) string {
	if provider == "ollama" {
		return "http://localhost:11434/v1"
	}
	return ""
}

// checkAgentInstalled mirrors install.sh's `command -v "$AGENT"` check
// against the target user's PATH, without actually executing the binary
// (some agent CLIs may require provider connectivity just to run
// --version, which isn't a fair thing to demand at preflight time).
func checkAgentInstalled(agent, runUser string) error {
	if _, err := runas.LookPath(runUser, agent); err != nil {
		return fmt.Errorf("%s is not installed for user %q: %w", agent, runUser, err)
	}
	return nil
}

func checkOllama() error {
	if err := exec.Command("systemctl", "is-active", "--quiet", "ollama").Run(); err != nil {
		return fmt.Errorf("ollama.service is not running; start it first: sudo systemctl enable --now ollama")
	}
	return nil
}

func installAgentmuxUnits(name, agent, provider, runUser, binPath, serviceName, updateServiceName, timerName string) error {
	unit := fmt.Sprintf(agentmuxUnitTemplate, name, agent, provider, runUser, binPath)
	updateUnit := fmt.Sprintf(agentmuxUpdateUnitTemplate, name, binPath)
	timer := fmt.Sprintf(agentmuxTimerTemplate, name, defaultOnCalendar)

	if err := os.WriteFile("/etc/systemd/system/"+serviceName, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+updateServiceName, []byte(updateUnit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+timerName, []byte(timer), 0o644); err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", serviceName); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", timerName); err != nil {
		return err
	}
	return nil
}
