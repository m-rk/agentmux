package gateway

import (
	"context"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// Backend is the session operations the gateway serves. LocalBackend is the
// real one; tests substitute a fake so they need no daemon.
type Backend interface {
	Host() string
	List(ctx context.Context) ([]ops.Session, error)
	Status(ctx context.Context, addr string) (ops.StatusResult, error)
	Threads(ctx context.Context, addr string) ([]transcript.Thread, error)
	Read(ctx context.Context, addr, cursor string, limit int) (transcript.Page, error)
	Send(ctx context.Context, req ops.SendRequest) ops.SendResult
	Create(ctx context.Context, req ops.CreateRequest) (ops.CreateResult, error)
	Run(ctx context.Context, req ops.RunRequest) (ops.RunResult, error)
	Retire(ctx context.Context, req ops.RetireRequest) (ops.RetireResult, error)
	GC(ctx context.Context, req ops.GCRequest) (ops.GCResult, error)
	// ShipPublish merges commits into the ship gate (AMUX-29) and
	// returns the gate. Versions reports the installed commit per
	// repo. SelfUpdateLog tails the updater's event log.
	ShipPublish(ctx context.Context, commits map[string]string) (map[string]string, error)
	Versions(ctx context.Context) (map[string]string, error)
	SelfUpdateLog(ctx context.Context, lines int) ([]string, error)
}

// LocalBackend serves this host's sessions through the local daemon.
type LocalBackend struct{ Env ops.Env }

func (LocalBackend) Host() string { return address.LocalHostName() }

func (b LocalBackend) List(ctx context.Context) ([]ops.Session, error) { return b.Env.List(ctx) }

func (b LocalBackend) Status(ctx context.Context, addr string) (ops.StatusResult, error) {
	return b.Env.Status(ctx, addr)
}

func (LocalBackend) Threads(ctx context.Context, addr string) ([]transcript.Thread, error) {
	return ops.Threads(ctx, addr)
}

func (LocalBackend) Read(ctx context.Context, addr, cursor string, limit int) (transcript.Page, error) {
	return ops.Read(ctx, addr, cursor, limit)
}

func (b LocalBackend) Send(ctx context.Context, req ops.SendRequest) ops.SendResult {
	return b.Env.Send(ctx, req)
}

func (b LocalBackend) Create(ctx context.Context, req ops.CreateRequest) (ops.CreateResult, error) {
	return b.Env.Create(ctx, req)
}

func (b LocalBackend) Run(ctx context.Context, req ops.RunRequest) (ops.RunResult, error) {
	return b.Env.Run(ctx, req)
}

func (b LocalBackend) Retire(ctx context.Context, req ops.RetireRequest) (ops.RetireResult, error) {
	return b.Env.Retire(ctx, req)
}

func (b LocalBackend) GC(ctx context.Context, req ops.GCRequest) (ops.GCResult, error) {
	return b.Env.GC(ctx, req)
}

// ShipPublish merges commits into this host's ship gate file, so the
// hub's publish lands where the pull updater reads it.
func (LocalBackend) ShipPublish(ctx context.Context, commits map[string]string) (map[string]string, error) {
	return ShipPublish(ctx, selfUpdateHome(), commits)
}

// Versions reports the installed commit per repo from this host's
// versions file.
func (LocalBackend) Versions(ctx context.Context) (map[string]string, error) {
	return InstalledVersions(ctx, selfUpdateHome())
}

// SelfUpdateLog tails this host's updater event log. lines <= 0 means
// the default tail; the reply is capped so a runaway log can't flood
// the gateway.
func (LocalBackend) SelfUpdateLog(_ context.Context, lines int) ([]string, error) {
	if lines <= 0 {
		lines = 50
	}
	if lines > 500 {
		lines = 500
	}
	return TailLog(selfUpdateHome(), lines)
}
