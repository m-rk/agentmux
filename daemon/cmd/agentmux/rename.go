package main

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

type renameDoneMsg struct{ err error }

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
