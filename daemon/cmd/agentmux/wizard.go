package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/user"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
	"github.com/m-rk/agentmux/daemon/internal/wizardui"
)

// runWizard is the `agentmux new` subcommand entrypoint: dial every
// configured host (same hosts.yaml/-socket fallback as the TUI) and either
// run the interactive form, or — if -y is given — create the instance
// directly from flags, for scripting (this repo's own migration off the
// bash-installed instances onto this provisioner was originally driven by
// a throwaway one-off CLI shaped exactly like this; promoted into the real
// tool instead of staying a script nobody else could reuse).
func runWizard(args []string) {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	nonInteractive := fs.Bool("y", false, "skip the interactive form; create directly from the flags below")
	host := fs.String("host", "local", "device to create the instance on (a name from hosts.yaml, or \"local\"); -y only")
	instance := fs.String("instance", "", "instance name; -y only")
	agent := fs.String("agent", "", "claude-code | zero | opencode | kilo; -y only")
	hostName := fs.String("host-name", "", "claude-code/kilo display hostname; blank = derive from the target device; -y only")
	provider := fs.String("provider", "", "zero/opencode/kilo only; \"ollama\" or a custom provider id; -y only")
	model := fs.String("model", "", "zero/opencode/kilo only; -y only")
	providerBaseURL := fs.String("provider-base-url", "", "zero/opencode/kilo only; required when -provider isn't \"ollama\"; -y only")
	providerAPIKeyEnv := fs.String("provider-api-key-env", "", "kilo/opencode only, not zero; optional; env var name (not the key itself) holding a custom provider's API key; -y only")
	workdir := fs.String("workdir", "", "blank = provisioner default; -y only")
	runUser := fs.String("run-user", "", "Linux only, required there; -y only")
	resume := fs.String("resume", "", "claude-code only, a session ID; -y only")
	compact := fs.String("compact", "", "claude-code only: \"\" (default/on) or \"off\"; -y only")
	force := fs.Bool("force", false, "allow re-provisioning the instance this process is currently running inside of; -y only")
	fs.Parse(args)

	if !*nonInteractive {
		hosts, err := loadHosts(*hostsPath, *socketPath)
		if err != nil {
			log.Fatalf("loading hosts: %v", err)
		}
		clients := map[string]*tuiclient.Client{}
		for _, h := range hosts {
			c, err := tuiclient.Dial(h.Name, h.Address)
			if err != nil {
				log.Fatalf("dialing %s (%s): %v", h.Name, h.Address, err)
			}
			clients[h.Name] = c
			defer c.Close()
		}
		if err := runWizardForm(clients); err != nil {
			log.Fatalf("new: %v", err)
		}
		return
	}

	if err := refuseIfSelfTarget(*host, *instance, *force); err != nil {
		log.Fatalf("new: %v", err)
	}

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("new: %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.CreateInstance(ctx, &pb.CreateInstanceRequest{
		InstanceName:      *instance,
		Agent:             *agent,
		HostName:          *hostName,
		Provider:          *provider,
		Model:             *model,
		Workdir:           *workdir,
		ResumeSessionId:   *resume,
		RunUser:           *runUser,
		CompactOnUpdate:   *compact,
		ProviderBaseUrl:   *providerBaseURL,
		ProviderApiKeyEnv: *providerAPIKeyEnv,
	})
	if err != nil {
		log.Fatalf("new: %v", err)
	}
	if !resp.Ok {
		log.Fatalf("new: %s", resp.Message)
	}
	fmt.Println(resp.Message)
}

type wizardDoneMsg struct{ err error }

// newInstanceCmd launches the wizard from inside the running TUI, reusing
// the same ReleaseTerminal/RestoreTerminal pattern attachCmd uses so it can
// take over the terminal in place, then dialing the same already-connected
// clients (no redial needed) once the form is filled in.
func newInstanceCmd(p *tea.Program, clients map[string]*tuiclient.Client) tea.Cmd {
	return func() tea.Msg {
		p.ReleaseTerminal()
		err := runWizardForm(clients)
		p.RestoreTerminal()
		return wizardDoneMsg{err: err}
	}
}

// runWizardForm first asks what to create and where, then builds a details
// form containing only fields that apply to the selected agent. It finally
// calls CreateInstance on the chosen host's client.
func runWizardForm(clients map[string]*tuiclient.Client) error {
	hostNames := make([]string, 0, len(clients))
	for name := range clients {
		hostNames = append(hostNames, name)
	}
	sort.Strings(hostNames)
	if len(hostNames) == 0 {
		return fmt.Errorf("no hosts available")
	}

	host := hostNames[0]
	agent := "claude-code"
	selectionForm := wizardui.NewSelectionForm(hostNames, &host, &agent)
	if err := selectionForm.Run(); err != nil {
		return err
	}

	client, ok := clients[host]
	if !ok {
		return fmt.Errorf("no connection to host %q", host)
	}

	capabilities := wizardui.CapabilitiesForAgent(agent)
	hostName := ""
	if capabilities.DisplayHostName {
		optionsCtx, optionsCancel := context.WithTimeout(context.Background(), 5*time.Second)
		options, optionsErr := client.GetCreateOptions(optionsCtx)
		optionsCancel()
		if optionsErr == nil {
			hostName = options.DefaultHostName
		}
	}

	details := &wizardui.Details{
		Instance: wizardui.DefaultInstance(agent),
		HostName: hostName,
		Provider: "ollama",
	}
	if u, err := user.Current(); err == nil {
		details.RunUser = u.Username
	}

	// Optional onboarding step: only offered if Discord notifications
	// aren't already configured, so returning wizard users creating a
	// second/third instance aren't asked again every time.
	includeDiscord := false
	if discordCfg, err := discordnotify.Load(discordnotify.DefaultPath()); err == nil {
		includeDiscord = discordCfg.WebhookURL == ""
	}
	form := wizardui.NewDetailsForm(agent, details, includeDiscord)
	if err := form.Run(); err != nil {
		return err
	}

	if details.SetupDiscord {
		if _, err := runDiscordSetupForm(); err != nil {
			fmt.Printf("warning: Discord setup failed, continuing without it: %v\n", err)
		}
	}

	// Only claude-code supports --resume, and the picker needs an explicit
	// workdir to look sessions up under — a blank workdir here means "use
	// the provisioner's own default," which this client can't predict for
	// an arbitrary remote device, so it's a fresh session in that case.
	resume := ""
	if agent == "claude-code" && details.Workdir != "" {
		resume = pickResumeSession(client, details.Workdir, details.RunUser)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.CreateInstance(ctx, &pb.CreateInstanceRequest{
		InstanceName:      details.Instance,
		Agent:             agent,
		HostName:          details.HostName,
		Provider:          details.Provider,
		Model:             details.Model,
		Workdir:           details.Workdir,
		ResumeSessionId:   resume,
		RunUser:           details.RunUser,
		CompactOnUpdate:   details.CompactOnUpdate,
		ProviderBaseUrl:   details.ProviderBaseURL,
		ProviderApiKeyEnv: details.ProviderAPIKeyEnv,
	})
	if err != nil {
		return err
	}
	if !resp.Ok {
		return fmt.Errorf("%s", resp.Message)
	}
	fmt.Println(resp.Message)
	return nil
}

// pickResumeSession looks up resumable Claude Code sessions for workdir on
// client's host and, if any exist, prompts for one via a picker (newest
// first). Returns "" (fresh session) if none are found, the lookup fails
// (treated as non-fatal — resume is an enhancement, not core to creating
// an instance), or the user picks "fresh session" explicitly.
func pickResumeSession(client *tuiclient.Client, workdir, runUser string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.ListResumableSessions(ctx, &pb.ListResumableSessionsRequest{Workdir: workdir, RunUser: runUser})
	if err != nil {
		fmt.Printf("warning: could not list resumable sessions: %v\n", err)
		return ""
	}
	if len(resp.Sessions) == 0 {
		return ""
	}

	options := []huh.Option[string]{huh.NewOption("fresh session (no resume)", "")}
	for _, s := range resp.Sessions {
		options = append(options, huh.NewOption(
			fmt.Sprintf("%s (%s)", s.SessionId, relativeTime(s.LastModifiedUnix)), s.SessionId))
	}

	resume := ""
	picker := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Resume session?").
			Description(fmt.Sprintf("found %d existing session(s) for %s", len(resp.Sessions), workdir)).
			Options(options...).
			Value(&resume),
	))
	if err := picker.Run(); err != nil {
		fmt.Printf("warning: resume picker failed, starting fresh: %v\n", err)
		return ""
	}
	return resume
}
