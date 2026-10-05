package ops

import (
	"context"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/retire"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// RetireRequest ends one finished task session; see the retire package.
type RetireRequest struct {
	Address string // <instance>@<host>
	DryRun  bool
}

// RetireResult is the retire outcome plus the address.
type RetireResult struct {
	retire.RetireResult
	Address string `json:"address"`
}

// Retire ends one finished task session on this host: archive the amp
// thread (or stop the local session), remove units and registry, remove
// the worktree, delete the branch when main contains it. Only task-*
// instances are touched; anything else is refused as forbidden. A dirty
// worktree or unmerged branch is refused as invalid — the caller raises
// an ask instead.
func (e Env) Retire(ctx context.Context, req RetireRequest) (RetireResult, error) {
	addr, err := parseLocal(req.Address)
	if err != nil {
		return RetireResult{}, err
	}
	if addr.Thread != "" {
		return RetireResult{}, Refuse(safesend.ReasonInvalid, "retire names a session, not a thread: %q", req.Address)
	}
	res, err := retire.Retire(ctx, retire.LiveEnv{}, addr.Instance, req.DryRun)
	if err != nil {
		return RetireResult{}, retireAsError(err)
	}
	return RetireResult{RetireResult: res, Address: addr.Session().String()}, nil
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
