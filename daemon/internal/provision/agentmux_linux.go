package provision

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// TimeoutStartSec=90 below (was 30): a genuinely-fine kilo cold start —
// fresh isolated login, cold model-list/indexing caches — confirmed live
// to take 34s, which the old 30s timeout killed with a bare "start
// operation timed out" and no indication it was just slow, not broken.
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
TimeoutStartSec=90

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

const agentmuxTickServiceTemplate = `[Unit]
Description=Periodic health check for agentmux instance %[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=%[2]s
ExecStart=%[3]s session run --instance %[1]s
TimeoutStartSec=90
`

const agentmuxTickTimerTemplate = `[Unit]
Description=Periodic health check timer for agentmux instance %[1]s

[Timer]
OnUnitActiveSec=%[2]d
OnBootSec=%[2]d

[Install]
WantedBy=timers.target
`

// createAgentmux is the native Go port of backends/agentmux/install.sh's
// Linux path (the "zero"/"opencode" + ollama backend), mirroring
// createClaudeCode's structure.
func createAgentmux(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		name = defaultInstanceName(opts.Agent, opts.Workdir)
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

	// Captured before writeRegistry below overwrites the file: whether this
	// is a fresh instance or a re-provision of an existing one determines
	// whether there's a live process whose stale config needs clearing out
	// (see the restart call near the end of this function).
	_, alreadyExisted := existingAgentFor(name)

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

	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}
	hostName := ""
	if opts.Agent == "kilo" {
		hostName, err = resolveHostName(opts.HostName)
		if err != nil {
			return "", err
		}
	}

	baseURL, err := resolveBaseURL(provider, opts.BaseURL)
	if err != nil {
		return "", err
	}

	if err := checkAgentInstalled(opts.Agent, runUser); err != nil {
		return "", err
	}
	if provider == "ollama" {
		if err := checkOllama(); err != nil {
			return "", err
		}
	}

	if err := ensureWorkdirForUser(workdir, u); err != nil {
		return "", err
	}

	serviceName := "agentmux-" + name + ".service"
	updateServiceName := "agentmux-" + name + "-update.service"
	timerName := "agentmux-" + name + "-update.timer"
	tickServiceName := "agentmux-" + name + "-tick.service"
	tickTimerName := "agentmux-" + name + "-tick.timer"

	// Stop the old process *before* touching anything on disk — separately
	// from the fact that "enable --now" below is a no-op on an
	// already-active oneshot unit (which alone would mean the new config
	// never actually gets picked up). This relies on StopAgentmux/ExecStop
	// (session.StopAgentmux) actually waiting for the old process to be
	// gone rather than just issuing the kill and returning — see its own
	// doc comment for the state-flush race that guards against. Only
	// matters for a genuine re-provision — a brand new instance has no
	// unit yet.
	if alreadyExisted {
		if err := runSystemctl("stop", serviceName); err != nil {
			return "", fmt.Errorf("stopping %s before applying updated config: %w", serviceName, err)
		}
	}

	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_AGENT", opts.Agent},
		{"AGENTMUX_PROVIDER", provider},
		{"AGENTMUX_MODEL", model},
		{"AGENTMUX_PROVIDER_BASE_URL", baseURL},
		{"AGENTMUX_PROVIDER_API_KEY_ENV", opts.APIKeyEnv},
		{"AGENTMUX_PROVIDER_WAIT_SECONDS", defaultProviderWaitSecs},
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

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	if err := installAgentmuxUnits(name, opts.Agent, provider, runUser, self, serviceName, updateServiceName, timerName, tickServiceName, tickTimerName); err != nil {
		return "", err
	}

	verb := "Created"
	if alreadyExisted {
		verb = "Updated"
	}
	message := fmt.Sprintf("%s instance %q (registry: %s). Reattach with: sudo -u %s tmux -L agentmux-%s attach -t %s",
		verb, name, regPath, runUser, name, sessionName)
	if opts.Agent == "kilo" && provider != "ollama" && opts.APIKeyEnv != "" {
		message += kiloCustomProviderNote(provider, baseURL, model, opts.APIKeyEnv)
	}
	return message, nil
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

func installAgentmuxUnits(name, agent, provider, runUser, binPath, serviceName, updateServiceName, timerName, tickServiceName, tickTimerName string) error {
	unit := fmt.Sprintf(agentmuxUnitTemplate, name, agent, provider, runUser, binPath)
	updateUnit := fmt.Sprintf(agentmuxUpdateUnitTemplate, name, binPath)
	timer := fmt.Sprintf(agentmuxTimerTemplate, name, defaultOnCalendar)
	tickService := fmt.Sprintf(agentmuxTickServiceTemplate, name, runUser, binPath)
	tickTimer := fmt.Sprintf(agentmuxTickTimerTemplate, name, defaultTickIntervalSecs)

	if err := os.WriteFile("/etc/systemd/system/"+serviceName, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+updateServiceName, []byte(updateUnit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+timerName, []byte(timer), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+tickServiceName, []byte(tickService), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+tickTimerName, []byte(tickTimer), 0o644); err != nil {
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
	if err := runSystemctl("enable", "--now", tickTimerName); err != nil {
		return err
	}
	return nil
}
