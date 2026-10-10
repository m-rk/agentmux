package daemonserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// RetireInstance ends a finished task session's managed half: stop the
// session, remove its units and registry entry. Only task-* instances are
// ever touched — anything else is refused, mirroring retire's guard —
// because this RPC deletes the instance's registration outright.
//
// The daemon owns this half because it is privileged on Linux: stopping
// touches the tmux server through the unit user, and the units
// (/etc/systemd/system) and registry (/etc/agentmux) are root-owned. The
// unprivileged `sessions retire` caller keeps the other half — archiving
// threads, git/worktree/branch work as the run user, writing the retired
// record — and calls this RPC for exactly this step, the way `sessions
// create` already delegates instance creation to CreateInstance.
func (s *Server) RetireInstance(ctx context.Context, req *pb.RetireInstanceRequest) (*pb.RetireInstanceResponse, error) {
	if req.Archive {
		if err := provision.ValidateInstanceName(req.Instance); err != nil {
			return &pb.RetireInstanceResponse{Ok: false, Message: err.Error()}, nil
		}
		instances, err := discovery.List()
		if err != nil {
			return &pb.RetireInstanceResponse{Ok: false, Message: fmt.Sprintf("listing instances: %v", err)}, nil
		}
		found := false
		for _, inst := range instances {
			if inst.Name == req.Instance {
				found = true
				break
			}
		}
		if !found {
			return &pb.RetireInstanceResponse{Ok: false, Message: fmt.Sprintf("no instance %q on this host", req.Instance)}, nil
		}
		archive, err := session.ArchiveManaged(req.Instance)
		if err != nil {
			return &pb.RetireInstanceResponse{Ok: false, Message: err.Error()}, nil
		}
		return &pb.RetireInstanceResponse{Ok: true, Message: fmt.Sprintf("removed %s: stopped and disabled instance; archived registry and service artifacts at %s", req.Instance, archive)}, nil
	}
	if !strings.HasPrefix(req.Instance, "task-") {
		return &pb.RetireInstanceResponse{Ok: false,
			Message: fmt.Sprintf("not a task session: %q does not start with %q; retire only touches task-* agents created by `sessions create`",
				req.Instance, "task-")}, nil
	}
	_ = ctx
	instances, err := discovery.List()
	if err != nil {
		return &pb.RetireInstanceResponse{Ok: false, Message: fmt.Sprintf("listing instances: %v", err)}, nil
	}
	found := false
	for _, inst := range instances {
		if inst.Name == req.Instance {
			found = true
			break
		}
	}
	if !found {
		return &pb.RetireInstanceResponse{Ok: false, Message: fmt.Sprintf("no instance %q on this host", req.Instance)}, nil
	}
	var notes []string
	if err := session.StopManaged(req.Instance); err != nil {
		// A stopped-but-registered instance reads dead either way; the
		// removal below is what actually retires it, so a stop failure is
		// reported, not fatal.
		notes = append(notes, fmt.Sprintf("stop: %v", err))
	}
	if err := session.RemoveUnits(req.Instance); err != nil {
		return &pb.RetireInstanceResponse{Ok: false, Message: err.Error()}, nil
	}
	if err := session.RemoveRegistry(req.Instance); err != nil {
		return &pb.RetireInstanceResponse{Ok: false, Message: err.Error()}, nil
	}
	msg := fmt.Sprintf("retired %s: stopped session, removed units and registry entry", req.Instance)
	if len(notes) > 0 {
		msg += " (" + strings.Join(notes, "; ") + ")"
	}
	return &pb.RetireInstanceResponse{Ok: true, Message: msg}, nil
}
