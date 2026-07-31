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
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
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
	provider := fs.String("provider", "", "zero/opencode/kilo only; -y only")
	model := fs.String("model", "", "zero/opencode/kilo only; -y only")
	workdir := fs.String("workdir", "", "blank = provisioner default; -y only")
	runUser := fs.String("run-user", "", "Linux only, required there; -y only")
	resume := fs.String("resume", "", "claude-code only, a session ID; -y only")
	compact := fs.String("compact", "", "claude-code only: \"\" (default/on) or \"off\"; -y only")
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

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("new: %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.CreateInstance(ctx, &pb.CreateInstanceRequest{
		InstanceName:    *instance,
		Agent:           *agent,
		Provider:        *provider,
		Model:           *model,
		Workdir:         *workdir,
		ResumeSessionId: *resume,
		RunUser:         *runUser,
		CompactOnUpdate: *compact,
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

// runWizardForm prompts for device/agent/instance details, then calls
// CreateInstance on the chosen host's client. claude-code (Linux only so
// far) and zero/opencode/kilo (also Linux only so far) are selectable;
// macOS provisioning isn't implemented yet and CreateInstance will report
// that clearly rather than doing something wrong.
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
		host            = hostNames[0]
		agent           = "claude-code"
		instance        = "claude-code"
		runUser         string
		workdir         string
		provider        string
		model           string
		compactOnUpdate string
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
					huh.NewOption("kilo", "kilo"),
				).
				Value(&agent),
		),
		huh.NewGroup(
			huh.NewInput().Title("Instance name").Value(&instance),
			huh.NewInput().Title("Run as user").Description("required; the device's OS username to run the session as").Value(&runUser),
			huh.NewInput().Title("Workdir").Description("blank = provisioner default").Value(&workdir),
			huh.NewInput().Title("Provider").Description("zero/opencode/kilo only; blank = ollama").Value(&provider),
			huh.NewInput().Title("Model").Description("zero/opencode/kilo only; blank = provisioner default").Value(&model),
			huh.NewSelect[string]().Title("Compact before nightly resume?").
				Description("claude-code only; prevents Claude Code's own huge-session resume prompt by compacting and restarting every night, not just on a version change").
				Options(
					huh.NewOption("on (default)", ""),
					huh.NewOption("off", "off"),
				).
				Value(&compactOnUpdate),
		),
	)
	if err := form.Run(); err != nil {
		return err
	}

	client, ok := clients[host]
	if !ok {
		return fmt.Errorf("no connection to host %q", host)
	}

	// Only claude-code supports --resume, and the picker needs an explicit
	// workdir to look sessions up under — a blank workdir here means "use
	// the provisioner's own default," which this client can't predict for
	// an arbitrary remote device, so it's a fresh session in that case.
	resume := ""
	if agent == "claude-code" && workdir != "" {
		resume = pickResumeSession(client, workdir, runUser)
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
		CompactOnUpdate: compactOnUpdate,
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
