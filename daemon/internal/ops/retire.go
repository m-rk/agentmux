package ops

import (
	"context"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/retire"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// RetireRequest ends one finished task session; see the retire package.
type RetireRequest struct {
	Address string // <instance>@<host>
	DryRun  bool
	// RequireMerged refuses the retire when the branch isn't provably
	// merged instead of retiring with the branch kept: the strict mode
	// for callers that want the old refuse-and-ask behavior.
	RequireMerged bool
}

// RetireResult is the retire outcome plus the address.
type RetireResult struct {
	retire.RetireResult
	Address string `json:"address"`
}

// Retire ends one finished task session on this host. The privileged half
// — stop the session, remove its units and registry entry — runs through
// the daemon (root on Linux), so this works unprivileged; the git half —
// archive threads, worktree and branch work as the run user, the retired
// record — runs here in the caller, never as root. A branch with commits
// not on origin is kept and reported, not a refusal; only a dirty
// worktree, a detached HEAD with unpushed commits, or -require-merged
// with an unmerged branch refuses as invalid. Only task-* instances are
// touched; anything else is refused as forbidden.
func (e Env) Retire(ctx context.Context, req RetireRequest) (RetireResult, error) {
	addr, err := parseLocal(req.Address)
	if err != nil {
		return RetireResult{}, err
	}
	if addr.Thread != "" {
		return RetireResult{}, Refuse(safesend.ReasonInvalid, "retire names a session, not a thread: %q", req.Address)
	}
	// The dry run touches nothing, so it needs no daemon: it inspects
	// as the run user and reports. A real retire dials the daemon for
	// the privileged managed half.
	var env retire.Env = retire.DaemonEnv{}
	if !req.DryRun {
		d, err := e.daemon()
		if err != nil {
			return RetireResult{}, err
		}
		defer d.Close()
		env = retire.DaemonEnv{Daemon: daemonRetireClient{ctx: ctx, d: d}}
	}
	res, err := retire.Retire(ctx, env, addr.Instance, retire.Options{DryRun: req.DryRun, RequireMerged: req.RequireMerged})
	if err != nil {
		return RetireResult{}, retireAsError(err)
	}
	return RetireResult{RetireResult: res, Address: addr.Session().String()}, nil
}

// daemonRetireClient adapts the daemon client to the retire package's
// narrow managed-half interface: it names no git paths and moves no
// bytes, only the instance name.
type daemonRetireClient struct {
	ctx context.Context
	d   Daemon
}

func (c daemonRetireClient) StopRemove(ctx context.Context, instance string) (string, error) {
	_ = ctx
	resp, err := c.d.RetireInstance(c.ctx, &pb.RetireInstanceRequest{Instance: instance})
	if err != nil {
		return "", err
	}
	if !resp.Ok {
		return "", retire.ManagedRefusal(resp.Message)
	}
	return resp.Message, nil
}

// GCRequest deletes the leftovers of retired sessions older than the host
// retention.
type GCRequest struct {
	DryRun bool
}

// GCResult is the gc outcome.
type GCResult struct {
	retire.GCResult
}

// GC deletes archived amp threads and stored opencode sessions retired
// longer ago than retention.yaml allows (default 14 days). Claude Code
// transcripts are never deleted.
func (e Env) GC(ctx context.Context, req GCRequest) (GCResult, error) {
	res, err := retire.GC(ctx, retire.LiveEnv{}, req.DryRun, time.Now())
	if err != nil {
		return GCResult{}, retireAsError(err)
	}
	return GCResult{GCResult: res}, nil
}

// retireAsError maps a retire refusal (stable safesend reason) into an
// ops refusal so the CLI and gateway shape it like every other error.
func retireAsError(err error) error {
	return Refuse(retire.ReasonOf(err), "%s", retire.DetailOf(err))
}
