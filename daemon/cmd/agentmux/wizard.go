package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// newInstanceCmd launches the "create a new instance" wizard, reusing the
// same ReleaseTerminal/RestoreTerminal pattern attachCmd uses so it can take
// over the terminal in place. Placeholder until the wizard itself lands.
func newInstanceCmd(p *tea.Program, clients map[string]*tuiclient.Client) tea.Cmd {
	return func() tea.Msg {
		return attachDoneMsg{err: fmt.Errorf("wizard not implemented yet")}
	}
}

// runWizard is the `agentmux new` subcommand entrypoint. Placeholder until
// the wizard itself lands.
func runWizard(args []string) {
	fmt.Println("agentmux new: not implemented yet")
}
