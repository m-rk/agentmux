// Package discovery finds agentmux instances by reading the env files that
// backends/agentmux/install.sh and backends/claude-code/install.sh write to
// /etc/agentmux, then cross-references them with tmux for liveness.
package discovery

import (
	"bufio"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	envDir        = "/etc/agentmux"
	idleThreshold = 30 * time.Second
)

type Status int

const (
	StatusUnknown Status = iota
	StatusRunning
	StatusIdle
	StatusDead
)

type Instance struct {
	Name         string
	Agent        string
	Provider     string
	Model        string
	Workdir      string
	TmuxSession  string
	TmuxSocket   string // -S path of the tmux server hosting TmuxSession, if found
	ServiceName  string
	Status       Status
	PID          int64
	LastActivity time.Time
}

// List reads every *.env file in /etc/agentmux and merges in tmux liveness.
func List() ([]Instance, error) {
	entries, err := os.ReadDir(envDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	panes, err := tmuxPanes()
	if err != nil {
		// tmux not installed or no server running yet: every instance is dead.
		panes = nil
	}

	var out []Instance
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		fields, err := parseEnvFile(filepath.Join(envDir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, instanceFromEnv(e.Name(), fields, panes))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func instanceFromEnv(filename string, fields map[string]string, panes map[string]tmuxPane) Instance {
	name := fields["AGENTMUX_INSTANCE_NAME"]
	if name == "" {
		name = strings.TrimSuffix(filename, ".env")
	}

	session := fields["AGENTMUX_TMUX_SESSION_NAME"]
	if session == "" {
		session = fields["AGENTMUX_SESSION_NAME"]
	}
	if session == "" {
		session = name
	}

	service := fields["AGENTMUX_SERVICE_NAME"]
	if service == "" {
		service = "agentmux-" + name + ".service"
	}

	agent := fields["AGENTMUX_AGENT"]
	if agent == "" {
		// backends/claude-code doesn't set AGENTMUX_AGENT; it's the only
		// other backend today.
		agent = "claude-code"
	}

	inst := Instance{
		Name:        name,
		Agent:       agent,
		Provider:    fields["AGENTMUX_PROVIDER"],
		Model:       fields["AGENTMUX_MODEL"],
		Workdir:     fields["AGENTMUX_WORKDIR"],
		TmuxSession: session,
		ServiceName: service,
		Status:      StatusDead,
	}

	if pane, ok := panes[session]; ok {
		inst.PID = pane.pid
		inst.LastActivity = pane.lastChanged
		inst.TmuxSocket = pane.socket
		if time.Since(pane.lastChanged) < idleThreshold {
			inst.Status = StatusRunning
		} else {
			inst.Status = StatusIdle
		}
	}

	return inst
}

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fields := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return fields, scanner.Err()
}

type tmuxPane struct {
	pid         int64
	lastChanged time.Time
	socket      string
}

// activityCache tracks, per session, a hash of the pane's last-seen content
// and when it last changed. tmux's own #{pane_activity} is only populated
// when a window has "monitor-activity on" set, which agentmux instances
// don't set and shouldn't have to — so instead we track it ourselves by
// diffing capture-pane output across polls.
var activityCache = struct {
	mu      sync.Mutex
	entries map[string]activityEntry
}{entries: map[string]activityEntry{}}

type activityEntry struct {
	hash        uint64
	lastChanged time.Time
}

func observeActivity(session string, content []byte) time.Time {
	h := fnv.New64a()
	h.Write(content)
	sum := h.Sum64()

	activityCache.mu.Lock()
	defer activityCache.mu.Unlock()

	now := time.Now()
	prev, ok := activityCache.entries[session]
	if !ok || prev.hash != sum {
		activityCache.entries[session] = activityEntry{hash: sum, lastChanged: now}
		return now
	}
	return prev.lastChanged
}

// tmuxPanes returns the lead pane per session, keyed by session name, across
// every tmux server on the host.
//
// tmux servers are per-user, and each agentmux instance now runs its own
// (named "agentmux-<instance>", not the user's default server — sharing one
// server across instances meant killing any single instance's systemd unit
// could SIGKILL the whole shared server via cgroup cleanup, taking every
// other instance down with it). agentmuxd typically runs as root (it needs
// to call systemctl), which has no tmux server of its own and can't see
// other users' sessions through a bare "tmux list-panes" — so instead this
// globs every per-user, per-instance socket directly. tmux lets root
// connect to any user's socket regardless of file ownership.
func tmuxPanes() (map[string]tmuxPane, error) {
	sockets, err := filepath.Glob("/tmp/tmux-*/agentmux-*")
	if err != nil {
		return nil, err
	}

	panes := map[string]tmuxPane{}
	var lastErr error
	for _, socket := range sockets {
		out, err := exec.Command("tmux", "-S", socket, "list-panes", "-a", "-F",
			"#{session_name}\t#{pane_pid}").Output()
		if err != nil {
			lastErr = err
			continue
		}
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		for scanner.Scan() {
			parts := strings.Split(scanner.Text(), "\t")
			if len(parts) != 2 {
				continue
			}
			session := parts[0]
			if _, exists := panes[session]; exists {
				continue // already recorded the lead pane for this session
			}
			pid, _ := strconv.ParseInt(parts[1], 10, 64)

			content, capErr := exec.Command("tmux", "-S", socket, "capture-pane", "-p", "-t", session).Output()
			var lastChanged time.Time
			if capErr == nil {
				lastChanged = observeActivity(session, content)
			}

			panes[session] = tmuxPane{pid: pid, lastChanged: lastChanged, socket: socket}
		}
	}
	if len(panes) == 0 {
		return panes, lastErr
	}
	return panes, nil
}
