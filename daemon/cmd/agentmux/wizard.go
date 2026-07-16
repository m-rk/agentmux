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

	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// runWizard is the `agentmux new` subcommand entrypoint: dial every
// configured host (same hosts.yaml/-socket fallback as the TUI) and run
// the interactive form.
func runWizard(args []string) {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	socketPath := fs.String("socket", "/run/agentmux/agentmuxd.sock", "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	fs.Parse(args)

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

// runWizardForm prompts for device/agent/instance details, then calls
// CreateInstance on the chosen host's client. claude-code (Linux only so
// far) and zero/opencode (also Linux only so far) are selectable; macOS
// provisioning isn't implemented yet and CreateInstance will report that
// clearly rather than doing something wrong.
func runWizardForm(clients map[string]*tuiclient.Client) error {
	hostNames := make([]string, 0, len(clients))
	for name := range clients {
		hostNames = append(hostNames, name)
	}
	sort.Strings(hostNames)
	if len(hostNames) == 0 {
		return fmt.Errorf("no hosts available")
	}

	hostOptions := make([]huh.Option[string], len(hostNames))
	for i, n := range hostNames {
		hostOptions[i] = huh.NewOption(n, n)
	}

	var (
		host     = hostNames[0]
		agent    = "claude-code"
		instance = "claude-code"
		runUser  string
		workdir  string
		resume   string
		provider string
		model    string
	)
	if u, err := user.Current(); err == nil {
		runUser = u.Username
	}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Device").Options(hostOptions...).Value(&host),
			huh.NewSelect[string]().Title("Agent").
				Options(
					huh.NewOption("claude-code", "claude-code"),
					huh.NewOption("zero", "zero"),
					huh.NewOption("opencode", "opencode"),
				).
				Value(&agent),
		),
		huh.NewGroup(
			huh.NewInput().Title("Instance name").Value(&instance),
			huh.NewInput().Title("Run as user").Description("required; the device's OS username to run the session as").Value(&runUser),
			huh.NewInput().Title("Workdir").Description("blank = provisioner default").Value(&workdir),
			huh.NewInput().Title("Resume session ID").Description("claude-code only; blank = fresh session").Value(&resume),
			huh.NewInput().Title("Provider").Description("zero/opencode only; blank = ollama").Value(&provider),
			huh.NewInput().Title("Model").Description("zero/opencode only; blank = provisioner default").Value(&model),
		),
	)
	if err := form.Run(); err != nil {
		return err
	}

	client, ok := clients[host]
	if !ok {
		return fmt.Errorf("no connection to host %q", host)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.CreateInstance(ctx, &pb.CreateInstanceRequest{
		InstanceName:    instance,
		Agent:           agent,
		Provider:        provider,
		Model:           model,
		Workdir:         workdir,
		ResumeSessionId: resume,
		RunUser:         runUser,
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
