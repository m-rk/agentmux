// Command agentmux is the TUI client: it lists agentmux instances reported
// by one or more agentmuxd daemons, lets you attach to a session's tmux
// pane, and drives start/stop/restart. Phase 2: connects to every host
// listed in hosts.yaml concurrently; falls back to a single local daemon
// over -socket if no hosts.yaml is found (phase 1 behavior).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// retryDelay is how long a host's event stream waits before reconnecting
// after an error (e.g. a Tailscale host that's temporarily unreachable).
const retryDelay = 5 * time.Second

func main() {
	socketPath := flag.String("socket", "/run/agentmux/agentmuxd.sock", "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := flag.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	flag.Parse()

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

	m := &model{clients: clients, hostErr: map[string]string{}}
	program := tea.NewProgram(m, tea.WithAltScreen())
	m.program = program

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for name, c := range clients {
		go streamHost(ctx, program, name, c)
	}

	if _, err := program.Run(); err != nil {
		log.Fatalf("tui: %v", err)
	}
}

// loadHosts reads hostsPath if it exists; otherwise it falls back to a
// single "local" host dialed over socketPath, matching phase 1.
func loadHosts(hostsPath, socketPath string) ([]hostsconfig.Host, error) {
	cfg, err := hostsconfig.Load(hostsPath)
	if errors.Is(err, os.ErrNotExist) {
		return []hostsconfig.Host{{Name: "local", Address: "unix://" + socketPath}}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("%s: no hosts configured", hostsPath)
	}
	return cfg.Hosts, nil
}

// streamHost feeds one host's events into the program, reconnecting with a
// fixed delay if the stream fails so one unreachable host doesn't take down
// the rest of the TUI.
func streamHost(ctx context.Context, p *tea.Program, host string, c *tuiclient.Client) {
	for {
		ch, err := c.StreamEvents(ctx)
		if err != nil {
			p.Send(hostErrMsg{host: host, err: err})
		} else {
			p.Send(hostErrMsg{host: host, err: nil})
			for ev := range ch {
				p.Send(eventMsg{host: host, ev: ev})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

type row struct {
	host string
	inst *pb.Instance
}

type eventMsg struct {
	host string
	ev   *pb.InstanceEvent
}
type hostErrMsg struct {
	host string
	err  error // nil clears a previously reported error for this host
}
type attachDoneMsg struct{ err error }
type controlDoneMsg struct {
	host     string
	instance string
	resp     *pb.ControlResponse
	err      error
}

type model struct {
	clients   map[string]*tuiclient.Client
	program   *tea.Program
	instances []row
	hostErr   map[string]string
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
		m.applyEvent(msg.host, msg.ev)
		return m, nil

	case hostErrMsg:
		if msg.err == nil {
			delete(m.hostErr, msg.host)
		} else {
			m.hostErr[msg.host] = msg.err.Error()
		}
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
			m.err = fmt.Sprintf("%s/%s: %v", msg.host, msg.instance, msg.err)
		} else if !msg.resp.Ok {
			m.err = fmt.Sprintf("%s/%s: %s", msg.host, msg.instance, msg.resp.Message)
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
			if r := m.selected(); r != nil {
				return m, controlCmd(m.clients[r.host], r.host, r.inst.Name, action)
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
		if r := m.selected(); r != nil {
			return m, attachCmd(m.program, m.clients[r.host], r.inst.Name)
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

func (m *model) selected() *row {
	if m.cursor < 0 || m.cursor >= len(m.instances) {
		return nil
	}
	return &m.instances[m.cursor]
}

func (m *model) applyEvent(host string, ev *pb.InstanceEvent) {
	switch ev.Type {
	case pb.EventType_EVENT_UPDATED:
		for i, r := range m.instances {
			if r.host == host && r.inst.Name == ev.Instance.Name {
				m.instances[i].inst = ev.Instance
				return
			}
		}
		m.instances = append(m.instances, row{host: host, inst: ev.Instance})
		sort.Slice(m.instances, func(i, j int) bool {
			if m.instances[i].host != m.instances[j].host {
				return m.instances[i].host < m.instances[j].host
			}
			return m.instances[i].inst.Name < m.instances[j].inst.Name
		})
	case pb.EventType_EVENT_REMOVED:
		for i, r := range m.instances {
			if r.host == host && r.inst.Name == ev.Instance.Name {
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

func controlCmd(client *tuiclient.Client, host, instance string, action pb.ControlAction) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, err := client.Control(ctx, instance, action)
		return controlDoneMsg{host: host, instance: instance, resp: resp, err: err}
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
	lines = append(lines, headerStyle.Render(fmt.Sprintf("%-10s %-16s %-12s %-24s %-8s %s", "HOST", "NAME", "AGENT", "MODEL", "STATUS", "WORKDIR")))

	if len(m.instances) == 0 {
		lines = append(lines, dimStyle.Render("no instances found"))
	}
	for i, r := range m.instances {
		inst := r.inst
		line := fmt.Sprintf("%-10s %-16s %-12s %-24s %-8s %s",
			r.host, inst.Name, inst.Agent, truncate(inst.Model, 24), statusLabel(inst.Status), inst.Workdir)
		line = statusStyle[inst.Status].Render(line)
		if i == m.cursor {
			line = selectedStyle.Render("> ") + line
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}

	if len(m.hostErr) > 0 {
		names := make([]string, 0, len(m.hostErr))
		for name := range m.hostErr {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			lines = append(lines, errStyle.Render(fmt.Sprintf("%s: %s (retrying)", name, m.hostErr[name])))
		}
	}

	lines = append(lines, "")
	switch {
	case m.confirm != pb.ControlAction_CONTROL_UNKNOWN:
		lines = append(lines, errStyle.Render(fmt.Sprintf("confirm %s on %q? (y/n)", confirmLabel(m.confirm), m.selected().inst.Name)))
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
