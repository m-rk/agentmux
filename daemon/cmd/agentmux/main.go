// Command agentmux is the TUI client: it lists agentmux instances reported
// by one or more agentmuxd daemons, lets you attach to a session's tmux
// pane, and drives start/stop/restart. Phase 1: a single local daemon over
// a Unix socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

func main() {
	socketPath := flag.String("socket", "/run/agentmux/agentmuxd.sock", "Unix socket agentmuxd is listening on")
	flag.Parse()

	client, err := tuiclient.Dial("local", *socketPath)
	if err != nil {
		log.Fatalf("connecting to agentmuxd: %v", err)
	}
	defer client.Close()

	m := &model{client: client}
	program := tea.NewProgram(m, tea.WithAltScreen())
	m.program = program

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch, err := client.StreamEvents(ctx)
		if err != nil {
			program.Send(fatalMsg{err})
			return
		}
		for ev := range ch {
			program.Send(eventMsg{ev})
		}
	}()

	if _, err := program.Run(); err != nil {
		log.Fatalf("tui: %v", err)
	}
}

type eventMsg struct{ ev *pb.InstanceEvent }
type fatalMsg struct{ err error }
type attachDoneMsg struct{ err error }
type controlDoneMsg struct {
	instance string
	resp     *pb.ControlResponse
	err      error
}

type model struct {
	client    *tuiclient.Client
	program   *tea.Program
	instances []*pb.Instance
	cursor    int
	confirm   pb.ControlAction // CONTROL_UNKNOWN when not confirming
	status    string
	err       string
	quitting  bool
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case eventMsg:
		m.applyEvent(msg.ev)
		return m, nil

	case fatalMsg:
		m.err = msg.err.Error()
		return m, nil

	case attachDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.status = "detached"
		}
		return m, nil

	case controlDoneMsg:
		if msg.err != nil {
			m.err = fmt.Sprintf("%s: %v", msg.instance, msg.err)
		} else if !msg.resp.Ok {
			m.err = fmt.Sprintf("%s: %s", msg.instance, msg.resp.Message)
		} else {
			m.status = msg.resp.Message
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirm != pb.ControlAction_CONTROL_UNKNOWN {
		switch msg.String() {
		case "y":
			action := m.confirm
			m.confirm = pb.ControlAction_CONTROL_UNKNOWN
			if inst := m.selected(); inst != nil {
				return m, controlCmd(m.client, inst.Name, action)
			}
			return m, nil
		default:
			m.confirm = pb.ControlAction_CONTROL_UNKNOWN
			return m, nil
		}
	}

	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.instances)-1 {
			m.cursor++
		}
	case "a":
		if inst := m.selected(); inst != nil {
			return m, attachCmd(m.program, m.client, inst.Name)
		}
	case "r":
		if m.selected() != nil {
			m.confirm = pb.ControlAction_CONTROL_RESTART
		}
	case "s":
		if m.selected() != nil {
			m.confirm = pb.ControlAction_CONTROL_STOP
		}
	case "x":
		if m.selected() != nil {
			m.confirm = pb.ControlAction_CONTROL_START
		}
	}
	m.err = ""
	m.status = ""
	return m, nil
}

func (m *model) selected() *pb.Instance {
	if m.cursor < 0 || m.cursor >= len(m.instances) {
		return nil
	}
	return m.instances[m.cursor]
}

func (m *model) applyEvent(ev *pb.InstanceEvent) {
	switch ev.Type {
	case pb.EventType_EVENT_UPDATED:
		for i, inst := range m.instances {
			if inst.Name == ev.Instance.Name {
				m.instances[i] = ev.Instance
				return
			}
		}
		m.instances = append(m.instances, ev.Instance)
		sort.Slice(m.instances, func(i, j int) bool { return m.instances[i].Name < m.instances[j].Name })
	case pb.EventType_EVENT_REMOVED:
		for i, inst := range m.instances {
			if inst.Name == ev.Instance.Name {
				m.instances = append(m.instances[:i], m.instances[i+1:]...)
				if m.cursor >= len(m.instances) && m.cursor > 0 {
					m.cursor--
				}
				return
			}
		}
	}
}

func attachCmd(p *tea.Program, client *tuiclient.Client, instance string) tea.Cmd {
	return func() tea.Msg {
		p.ReleaseTerminal()
		err := client.AttachInteractive(context.Background(), instance)
		p.RestoreTerminal()
		return attachDoneMsg{err: err}
	}
}

func controlCmd(client *tuiclient.Client, instance string, action pb.ControlAction) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, err := client.Control(ctx, instance, action)
		return controlDoneMsg{instance: instance, resp: resp, err: err}
	}
}

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	statusStyle   = map[pb.Status]lipgloss.Style{
		pb.Status_STATUS_RUNNING: lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		pb.Status_STATUS_IDLE:    lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		pb.Status_STATUS_DEAD:    lipgloss.NewStyle().Foreground(lipgloss.Color("196")),
		pb.Status_STATUS_UNKNOWN: lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
	}
	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func statusLabel(s pb.Status) string {
	switch s {
	case pb.Status_STATUS_RUNNING:
		return "running"
	case pb.Status_STATUS_IDLE:
		return "idle"
	case pb.Status_STATUS_DEAD:
		return "dead"
	default:
		return "unknown"
	}
}

func (m *model) View() string {
	if m.quitting {
		return ""
	}

	var lines []string
	lines = append(lines, headerStyle.Render(fmt.Sprintf("%-16s %-12s %-24s %-8s %s", "NAME", "AGENT", "MODEL", "STATUS", "WORKDIR")))

	if len(m.instances) == 0 {
		lines = append(lines, dimStyle.Render("no instances found"))
	}
	for i, inst := range m.instances {
		line := fmt.Sprintf("%-16s %-12s %-24s %-8s %s",
			inst.Name, inst.Agent, truncate(inst.Model, 24), statusLabel(inst.Status), inst.Workdir)
		line = statusStyle[inst.Status].Render(line)
		if i == m.cursor {
			line = selectedStyle.Render("> ") + line
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}

	lines = append(lines, "")
	switch {
	case m.confirm != pb.ControlAction_CONTROL_UNKNOWN:
		lines = append(lines, errStyle.Render(fmt.Sprintf("confirm %s on %q? (y/n)", confirmLabel(m.confirm), m.selected().Name)))
	case m.err != "":
		lines = append(lines, errStyle.Render("error: "+m.err))
	case m.status != "":
		lines = append(lines, dimStyle.Render(m.status))
	}

	lines = append(lines, dimStyle.Render("a attach  r restart  s stop  x start  q quit"))
	return strings.Join(lines, "\n")
}

func confirmLabel(a pb.ControlAction) string {
	switch a {
	case pb.ControlAction_CONTROL_START:
		return "start"
	case pb.ControlAction_CONTROL_STOP:
		return "stop"
	case pb.ControlAction_CONTROL_RESTART:
		return "restart"
	default:
		return "?"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
