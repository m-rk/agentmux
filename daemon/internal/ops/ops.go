// Package ops is the session operations an orchestrator uses on one host:
// list, status, threads, read and send. The CLI (`agentmux list`,
// `agentmux sessions ...`), the gateway service, and later the MCP server and
// the SSH fallback all call these, so they behave the same everywhere. See
// docs/design/gateway.md.
//
// Every operation acts on this host only. An address naming another host is
// refused with ReasonNotLocal; reaching it is the gateway client's job.
package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// Error is a refusal with a stable reason; callers branch on Reason.
type Error struct {
	Reason safesend.Reason `json:"reason"`
	Detail string          `json:"detail"`
}

func (e *Error) Error() string { return string(e.Reason) + ": " + e.Detail }

// Refuse builds an *Error.
func Refuse(r safesend.Reason, format string, a ...any) error {
	return &Error{Reason: r, Detail: fmt.Sprintf(format, a...)}
}

// AsError turns any error into an *Error, treating unknown errors as
// ReasonFailed.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Reason: safesend.ReasonFailed, Detail: err.Error()}
}

// Env is where the local daemon is.
type Env struct {
	SocketPath string

	// Dial and Git replace the daemon client and the git runner that Create
	// uses; nil means the real ones. Tests set them.
	Dial func() (Daemon, error)
	Git  GitFunc
}

func (e Env) dial() (*tuiclient.Client, error) {
	return tuiclient.Dial("local", "unix://"+e.SocketPath)
}

// Session is one instance as an orchestrator sees it. It deliberately omits
// tmux and process details.
type Session struct {
	Address          string `json:"address"`
	Name             string `json:"name"`
	Agent            string `json:"agent"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	Status           string `json:"status"` // running, idle, dead
	Workdir          string `json:"workdir"`
	Project          string `json:"project,omitempty"` // see ProjectOf
	LastActivityUnix int64  `json:"last_activity_unix,omitempty"`
	StartedAtUnix    int64  `json:"started_at_unix,omitempty"`
}

// StatusLabel is the text form of a daemon status.
func StatusLabel(s pb.Status) string {
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

// SessionFrom converts a daemon instance on host (an address host name).
func SessionFrom(host string, inst *pb.Instance) Session {
	return Session{
		Address:          address.Address{Instance: inst.Name, Host: host}.String(),
		Name:             inst.Name,
		Agent:            inst.Agent,
		Provider:         inst.Provider,
		Model:            inst.Model,
		Status:           StatusLabel(inst.Status),
		Workdir:          inst.Workdir,
		LastActivityUnix: inst.LastActivityUnix,
		StartedAtUnix:    inst.StartedAtUnix,
	}
}

// List returns every instance on this host.
func (e Env) List(ctx context.Context) ([]Session, error) {
	c, err := e.dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	instances, err := c.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	host := address.LocalHostName()
	keys := ProjectKeys()
	out := make([]Session, 0, len(instances))
	for _, inst := range instances {
		s := SessionFrom(host, inst)
		s.Project = ProjectOf(inst.Name, inst.Workdir, keys)
		out = append(out, s)
	}
	return out, nil
}

// StatusResult is a session plus whether it can take a message now.
type StatusResult struct {
	Session
	// State is ready, busy, prompt, draft or unknown for TUI agents (from the
	// pane), or amp's own thread state (idle, ...) for an amp thread address.
	State  string `json:"state"`
	Thread string `json:"thread,omitempty"`
	// AmpMode is the effective amp mode for amp instances, and where it
	// came from ("instance" or "host"); absent for other agents and when
	// no mode is configured anywhere. See AMUX-15.
	AmpMode AmpModeInfo `json:"amp_mode,omitempty"`
	// Run is the run-thread state (running, done, or failed with Reason)
	// when the address names a thread started by `sessions run`.
	Run *RunStateInfo `json:"run,omitempty"`
}

// RunStateInfo is the stream-log state of a `sessions run` thread.
type RunStateInfo struct {
	// State is "running", "done", "waiting", or "failed".
	State string `json:"state"`
	// Reason is set when State is "failed".
	Reason string `json:"reason,omitempty"`
	// WaitingOn is the pending `ask_user_choice` question when State is
	// "waiting". It mirrors session.AmpWaitingOn.
	WaitingOn *WaitingOnInfo `json:"waiting_on,omitempty"`
}

// WaitingOnInfo is a pending question the agent asked through amp's
// built-in `ask_user_choice` tool: answering it needs `sessions run
// -thread <id>` with the choice.
type WaitingOnInfo struct {
	// Tool is always "ask_user_choice".
	Tool string `json:"tool"`
	// ToolUseID is the stream tool_use id (TU-…) the answer addresses.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Question is the question the agent asked.
	Question string `json:"question,omitempty"`
	// Options are the choices the agent offered.
	Options []string `json:"options,omitempty"`
	// AllowOther reports whether the agent also accepts a free-text
	// answer outside the options.
	AllowOther bool `json:"allow_other,omitempty"`
}

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Status looks up one session and reads its readiness.
func (e Env) Status(ctx context.Context, addrText string) (StatusResult, error) {
	addr, err := parseLocal(addrText)
	if err != nil {
		return StatusResult{}, err
	}
	c, err := e.dial()
	if err != nil {
		return StatusResult{}, err
	}
	defer c.Close()
	instances, err := c.ListInstances(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	for _, inst := range instances {
		if inst.Name != addr.Instance {
			continue
		}
		res := StatusResult{Session: SessionFrom(addr.Host, inst), State: string(safesend.StateUnknown), Thread: addr.Thread}
		res.Project = ProjectOf(inst.Name, inst.Workdir, ProjectKeys())
		if inst.Status == pb.Status_STATUS_DEAD || inst.TmuxSession == "" {
			res.State = "dead"
			return res, nil
		}
		if inst.Agent == "amp" {
			res.AmpMode = AmpModeOf(inst.Name)
			if addr.Thread != "" {
				src, _, err := Source(addr)
				if err != nil {
					return res, err
				}
				// A run thread's own stream log knows running/done/waiting/failed;
				// otherwise fall back to amp's last-known agent state.
				logPath := session.AmpRunLogPath(src.Home, addr.Instance, addr.Thread)
				if runState := session.AmpRunStateOf(logPath); runState.ThreadID == addr.Thread || fileExists(logPath) {
					res.Run = &RunStateInfo{State: runState.State, Reason: runState.Reason}
					if runState.WaitingOn != nil {
						res.Run.WaitingOn = &WaitingOnInfo{
							Tool: runState.WaitingOn.Tool, ToolUseID: runState.WaitingOn.ToolUseID,
							Question: runState.WaitingOn.Question, Options: runState.WaitingOn.Options,
							AllowOther: runState.WaitingOn.AllowOther,
						}
					}
					switch runState.State {
					case "done":
						res.State = "done"
					case "failed":
						res.State = "failed"
					case "waiting":
						// Waiting is not done: the run process is still
						// alive behind the pending question, so the
						// thread-level state stays running while run
						// carries the question.
						res.State = "running"
					default:
						res.State = "running"
					}
					return res, nil
				}
				state, err := transcript.AmpThreadState(ctx, src, addr.Thread)
				if errors.Is(err, transcript.ErrNoThread) {
					return res, Refuse(safesend.ReasonNotFound, "thread %s is not on this runner", addr.Thread)
				}
				if err != nil {
					return res, err
				}
				res.State = state
			}
			return res, nil
		}
		pane, err := c.ViewPane(ctx, &pb.ViewPaneRequest{Instance: inst.Name, Escapes: true})
		if err != nil {
			return res, err
		}
		res.State = string(safesend.Classify(inst.Agent, pane.Content))
		return res, nil
	}
	return StatusResult{}, Refuse(safesend.ReasonNotFound, "no instance %q on this host", addr.Instance)
}

// parseLocal parses an address and refuses one for another host.
func parseLocal(text string) (address.Address, error) {
	addr, err := address.Parse(text)
	if err != nil {
		return address.Address{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	if addr.Host != address.LocalHostName() {
		return addr, Refuse(safesend.ReasonNotLocal, "%s is not this host (%s); reaching another host's sessions needs the gateway", addr.Host, address.LocalHostName())
	}
	return addr, nil
}

// Source builds the transcript source for a local address from the
// instance's registry entry.
func Source(addr address.Address) (transcript.Source, transcript.Reader, error) {
	fields, err := session.ReadRegistry(addr.Instance)
	if err != nil {
		return transcript.Source{}, nil, Refuse(safesend.ReasonNotFound, "no instance %q on this host", addr.Instance)
	}
	src := transcript.Source{
		Instance:    addr.Instance,
		Agent:       fields["AGENTMUX_AGENT"],
		Workdir:     fields["AGENTMUX_WORKDIR"],
		Home:        runas.CurrentUserHome(),
		AmpRunnerID: fields["AGENTMUX_AMP_RUNNER_ID"],
	}
	if src.Agent == "" {
		src.Agent = "claude-code" // claude-code registry entries predate AGENTMUX_AGENT
	}
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := user.Lookup(runUser); err == nil {
			src.Home = u.HomeDir
		}
	}
	if src.Agent == "amp" {
		envFile := filepath.Join(src.Home, ".agentmux", "env", addr.Instance+".env")
		if info, err := os.Stat(envFile); err == nil && info.Mode().IsRegular() {
			src.AmpEnvFile = envFile
		}
	}
	r, err := transcript.For(src.Agent)
	if err != nil {
		return transcript.Source{}, nil, Refuse(safesend.ReasonUnsupported, "%v", err)
	}
	return src, r, nil
}

// Threads lists a local session's threads, newest first.
func Threads(ctx context.Context, addrText string) ([]transcript.Thread, error) {
	addr, err := parseLocal(addrText)
	if err != nil {
		return nil, err
	}
	src, r, err := Source(addr)
	if err != nil {
		return nil, err
	}
	threads, err := r.Threads(ctx, src)
	return threads, transcriptError(err)
}

// Read pages through a local session's transcript; the address's thread, if
// any, picks the thread.
func Read(ctx context.Context, addrText, cursor string, limit int) (transcript.Page, error) {
	addr, err := parseLocal(addrText)
	if err != nil {
		return transcript.Page{}, err
	}
	src, r, err := Source(addr)
	if err != nil {
		return transcript.Page{}, err
	}
	page, err := r.Read(ctx, src, addr.Thread, cursor, limit)
	return page, transcriptError(err)
}

func transcriptError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, transcript.ErrNoThread):
		return Refuse(safesend.ReasonNotFound, "%v", err)
	case errors.Is(err, transcript.ErrBadCursor):
		return Refuse(safesend.ReasonInvalid, "%v", err)
	case errors.Is(err, transcript.ErrUnsupported):
		return Refuse(safesend.ReasonUnsupported, "%v", err)
	}
	return err
}
