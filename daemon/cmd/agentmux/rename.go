package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

type renameDoneMsg struct{ err error }

// runRenameCmd is the `agentmux rename` subcommand entrypoint — the
// non-interactive, scriptable counterpart to the TUI's `R` keybinding.
func runRenameCmd(args []string) {
	fs := flag.NewFlagSet("rename", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "local", "device the instance lives on (a name from hosts.yaml, or \"local\")")
	instance := fs.String("instance", "", "instance name (required)")
	tmuxName := fs.String("tmux-name", "", "new tmux session name; blank = unchanged")
	displayName := fs.String("display-name", "", "new Remote Control display name (claude-code only); blank = unchanged. Restarts the session.")
	fs.Parse(args)

	if *instance == "" {
		log.Fatal("rename: -instance is required")
	}
	if *tmuxName == "" && *displayName == "" {
		log.Fatal("rename: at least one of -tmux-name / -display-name is required")
	}

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("rename: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.RenameInstance(ctx, &pb.RenameInstanceRequest{
		Instance:        *instance,
		TmuxSessionName: *tmuxName,
		DisplayName:     *displayName,
	})
	if err != nil {
		log.Fatalf("rename: %v", err)
	}
	if !resp.Ok {
		log.Fatalf("rename: %s", resp.Message)
	}
	fmt.Println(resp.Message)
}

// renameInstanceCmd launches a small form (device/agent already fixed —
// this is a single selected row, not a new-instance wizard) to rename r's
// tmux session and/or (claude-code only) its Remote Control display name,
// pre-filled with the current values. Same ReleaseTerminal/RestoreTerminal
// pattern as attachCmd/newInstanceCmd.
func renameInstanceCmd(p *tea.Program, client *tuiclient.Client, r row) tea.Cmd {
	return func() tea.Msg {
		p.ReleaseTerminal()
		err := runRenameForm(client, r)
		p.RestoreTerminal()
		return renameDoneMsg{err: err}
	}
}

func runRenameForm(client *tuiclient.Client, r row) error {
	tmuxName := r.inst.TmuxSession
	displayName := ""
	isClaudeCode := r.inst.Agent == "claude-code"

	fields := []huh.Field{
		huh.NewInput().Title("Tmux session name").Value(&tmuxName),
	}
	if isClaudeCode {
		fields = append(fields, huh.NewInput().
			Title("Display name").
			Description("Remote Control display name; blank = leave unchanged. Changing this restarts the session.").
			Value(&displayName))
	}

	form := huh.NewForm(huh.NewGroup(fields...))
	if err := form.Run(); err != nil {
		return err
	}

	req := &pb.RenameInstanceRequest{Instance: r.inst.Name}
	if tmuxName != r.inst.TmuxSession {
		req.TmuxSessionName = tmuxName
	}
	if displayName != "" {
		req.DisplayName = displayName
	}
	if req.TmuxSessionName == "" && req.DisplayName == "" {
		fmt.Println("nothing changed")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.RenameInstance(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Ok {
		return fmt.Errorf("%s", resp.Message)
	}
	fmt.Println(resp.Message)
	return nil
}
