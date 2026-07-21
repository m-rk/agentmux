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

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// retryDelay is how long a host's event stream waits before reconnecting
// after an error (e.g. a Tailscale host that's temporarily unreachable).
const retryDelay = 5 * time.Second

// runTUI is the default `agentmux` action: connect to every host listed in
// hosts.yaml concurrently, falling back to a single local daemon over
// -socket if no hosts.yaml is found (phase 1 behavior).
func runTUI(args []string) {
	fs := flag.NewFlagSet("agentmux", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
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

	if local, ok := clients["local"]; ok {
		if _, err := local.ListInstances(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not reach the local agentmuxd at %s (%v)\n", *socketPath, err)
			fmt.Fprintln(os.Stderr, "run 'agentmux daemon install' to set it up, then try again")
		}
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

// dialOneHost is loadHosts plus picking and dialing a single named entry —
// for CLI subcommands (rename, resume-list, new -y) that target exactly
// one device rather than the TUI's dial-everything-and-merge behavior.
func dialOneHost(hostsPath, socketPath, hostName string) (*tuiclient.Client, error) {
	hosts, err := loadHosts(hostsPath, socketPath)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if h.Name == hostName {
			return tuiclient.Dial(h.Name, h.Address)
		}
	}
	return nil, fmt.Errorf("host %q not found in %s", hostName, hostsPath)
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
	width     int
	height    int
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

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

	case wizardDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.status = "instance created"
		}
		return m, nil

	case renameDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.status = "renamed"
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
	case "n":
		return m, newInstanceCmd(m.program, m.clients)
	case "R":
		if r := m.selected(); r != nil {
			return m, renameInstanceCmd(m.program, m.clients[r.host], *r)
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

// Fixed column widths for the table. WORKDIR gets whatever's left of the
// terminal width, since it's the last column and tends to be the longest.
const (
	colHost   = 10
	colName   = 16
	colAgent  = 12
	colModel  = 20
	colStatus = 8
	// 5 columns + "> "/"  " prefix + one space separator after each fixed column.
	fixedColsWidth  = 2 + colHost + 1 + colName + 1 + colAgent + 1 + colModel + 1 + colStatus + 1
	minWorkdirWidth = 12
	defaultWidth    = 120 // used before the first WindowSizeMsg arrives
)

func (m *model) workdirWidth() int {
	w := m.width
	if w == 0 {
		w = defaultWidth
	}
	if avail := w - fixedColsWidth; avail > minWorkdirWidth {
		return avail
	}
	return minWorkdirWidth
}

func (m *model) View() string {
	if m.quitting {
		return ""
	}

	workdirWidth := m.workdirWidth()
	var lines []string
	lines = append(lines, "  "+headerStyle.Render(fmt.Sprintf("%-*s %-*s %-*s %-*s %-*s %-*s",
		colHost, "HOST", colName, "NAME", colAgent, "AGENT", colModel, "MODEL", colStatus, "STATUS", workdirWidth, "WORKDIR")))

	if len(m.instances) == 0 {
		lines = append(lines, dimStyle.Render("no instances found"))
	}
	for i, r := range m.instances {
		inst := r.inst
		line := fmt.Sprintf("%-*s %-*s %-*s %-*s %-*s %-*s",
			colHost, truncate(r.host, colHost),
			colName, truncate(inst.Name, colName),
			colAgent, truncate(inst.Agent, colAgent),
			colModel, truncate(inst.Model, colModel),
			colStatus, statusLabel(inst.Status),
			workdirWidth, truncate(inst.Workdir, workdirWidth))
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
	lines = append(lines, m.detailPanel()...)

	lines = append(lines, "")
	switch {
	case m.confirm != pb.ControlAction_CONTROL_UNKNOWN:
		lines = append(lines, errStyle.Render(fmt.Sprintf("confirm %s on %q? (y/n)", confirmLabel(m.confirm), m.selected().inst.Name)))
	case m.err != "":
		lines = append(lines, errStyle.Render("error: "+m.err))
	case m.status != "":
		lines = append(lines, dimStyle.Render(m.status))
	}

	lines = append(lines, dimStyle.Render("a attach  n new  R rename  r restart  s stop  x start  q quit"))
	return strings.Join(lines, "\n")
}

// detailPanel renders full, untruncated details for the selected instance,
// plus fields that don't fit in the table at all (tmux session, pid,
// relative start/last-activity times).
func (m *model) detailPanel() []string {
	r := m.selected()
	if r == nil {
		return []string{dimStyle.Render(strings.Repeat("─", 40)), dimStyle.Render("no instance selected")}
	}
	inst := r.inst

	sep := dimStyle.Render(strings.Repeat("─", 40))
	field := func(label, value string) string {
		if value == "" {
			value = "-"
		}
		return dimStyle.Render(label+": ") + value
	}

	lines := []string{sep}
	lines = append(lines, field("host/name", fmt.Sprintf("%s / %s", r.host, inst.Name)))
	lines = append(lines, field("agent", fmt.Sprintf("%s (provider: %s)", orDash(inst.Agent), orDash(inst.Provider))))
	lines = append(lines, field("model", orDash(inst.Model)))
	lines = append(lines, field("workdir", orDash(inst.Workdir)))
	lines = append(lines, field("tmux session", orDash(inst.TmuxSession)))
	pid := "-"
	if inst.Pid != 0 {
		pid = fmt.Sprintf("%d", inst.Pid)
	}
	lines = append(lines, field("pid", pid))
	lines = append(lines, field("started", relativeTime(inst.StartedAtUnix)))
	lines = append(lines, field("last activity", relativeTime(inst.LastActivityUnix)))
	return lines
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// relativeTime renders a "3h12m ago (2026-07-14 12:00:00)" style string, or
// "n/a" for a zero/unset timestamp (e.g. an instance with no live session).
func relativeTime(unixSec int64) string {
	if unixSec == 0 {
		return "n/a"
	}
	t := time.Unix(unixSec, 0)
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago (%s)", humanizeDuration(d), t.Local().Format("2006-01-02 15:04:05"))
}

func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd%dh", days, int(d.Hours())%24)
	}
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
