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
