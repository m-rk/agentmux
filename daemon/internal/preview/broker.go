package preview

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// Target is what the broker resolves for an authorised capture: everything
// comes from the instance's registry, nothing from the request but the folder.
type Target struct {
	Workspace string
	RunUser   string
}

// Authorize decides whether the caller (a uid taken from the request file's
// owner) may capture for instance, and where its workspace is. Only task-*
// instances qualify, the registry must name the instance, and the caller must
// be the user the task runs as. readRegistry and userID are injected so the
// broker's one trust decision is testable.
func Authorize(instance, callerUID string, readRegistry func(string) (map[string]string, error), userID func(string) (string, error)) (Target, error) {
	if callerUID == "" || callerUID == "0" {
		return Target{}, errors.New("preview capture needs a non-root caller")
	}
	if !strings.HasPrefix(instance, "task-") || strings.ContainsAny(instance, "/\\\x00\r\n") {
		return Target{}, errors.New("preview capture is available only to task-* instances")
	}
	fields, err := readRegistry(instance)
	if err != nil {
		return Target{}, fmt.Errorf("reading task registry: %w", err)
	}
	if fields["AGENTMUX_INSTANCE_NAME"] != instance {
		return Target{}, fmt.Errorf("registry identity does not match instance %q", instance)
	}
	workdir := strings.TrimSpace(fields["AGENTMUX_WORKDIR"])
	if !filepath.IsAbs(workdir) || strings.ContainsAny(workdir, "\x00\r\n") {
		return Target{}, errors.New("task registry has no valid absolute workdir")
	}
	runUser := strings.TrimSpace(fields["AGENTMUX_RUN_USER"])
	if runUser == "" {
		return Target{}, errors.New("task registry has no run user")
	}
	uid, err := userID(runUser)
	if err != nil {
		return Target{}, fmt.Errorf("looking up task user: %w", err)
	}
	if uid != callerUID {
		return Target{}, errors.New("caller does not own the registered task")
	}
	return Target{Workspace: filepath.Clean(workdir), RunUser: runUser}, nil
}

// Gate enforces one capture at a time per instance and a small host-wide cap,
// since each capture starts a browser.
type Gate struct {
	mu       sync.Mutex
	busy     map[string]bool
	inflight int
	max      int
}

// NewGate allows at most max captures at once across instances.
func NewGate(max int) *Gate { return &Gate{busy: map[string]bool{}, max: max} }

// Acquire reserves instance's slot. It returns a release func, or a reason it
// could not.
func (g *Gate) Acquire(instance string) (release func(), reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[instance] {
		return nil, "a capture for this instance is already running"
	}
	if g.inflight >= g.max {
		return nil, "the host is busy with other captures; try again shortly"
	}
	g.busy[instance] = true
	g.inflight++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.busy, instance)
		g.inflight--
	}, ""
}
