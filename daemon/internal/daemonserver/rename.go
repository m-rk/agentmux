package daemonserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// RenameInstance updates an instance's tmux session name and/or (claude-code
// only) its Remote Control display name.
//
// A tmux session name change is applied live via "tmux rename-session" if
// the session is currently running — cheap and non-disruptive, no restart
// needed. A display name change can't be applied live: it's baked into the
// claude --remote-control argv at launch, so it requires a restart, done
// here via applyControl's "restart" (the same OS-specific path Control's
// own restart action uses). That restart is issued by this daemon process
// — which is not part of the instance's own tmux session/process tree, even
// when renaming the instance hosting the caller's own conversation — so it
// doesn't have the self-referential "the shell issuing the restart dies
// along with the session it's restarting" problem that hand-driving a
// stop-then-start from *inside* the target session over two separate shell
// commands does (see docs/design/daemon-tui.md's migration notes).
func (s *Server) RenameInstance(ctx context.Context, req *pb.RenameInstanceRequest) (*pb.RenameInstanceResponse, error) {
	instances, err := discovery.List()
	if err != nil {
		return nil, err
	}
	var inst *discovery.Instance
	for i := range instances {
		if instances[i].Name == req.Instance {
			inst = &instances[i]
			break
		}
	}
	if inst == nil {
		return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("unknown instance %q", req.Instance)}, nil
	}

	var applied []string

	if req.TmuxSessionName != "" && req.TmuxSessionName != inst.TmuxSession {
		if inst.TmuxSocket != "" {
			if out, err := runas.CurrentUserCommand("tmux", "-S", inst.TmuxSocket, "rename-session",
				"-t", inst.TmuxSession, req.TmuxSessionName).CombinedOutput(); err != nil {
				return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("renaming tmux session: %v: %s", err, out)}, nil
			}
		}
		if err := session.SetRegistryField(inst.Name, "AGENTMUX_SESSION_NAME", req.TmuxSessionName); err != nil {
			return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("updating registry: %v", err)}, nil
		}
		if err := session.SetRegistryField(inst.Name, "AGENTMUX_TMUX_SESSION_NAME", req.TmuxSessionName); err != nil {
			return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("updating registry: %v", err)}, nil
		}
		applied = append(applied, fmt.Sprintf("tmux session -> %q", req.TmuxSessionName))
	}

	if req.DisplayName != "" {
		if inst.Agent != "claude-code" {
			return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("display name is only supported for claude-code instances, not %q", inst.Agent)}, nil
		}
		if err := session.SetRegistryField(inst.Name, "AGENTMUX_DISPLAY_NAME", req.DisplayName); err != nil {
			return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("updating registry: %v", err)}, nil
		}
		ok, msg := applyControl(ctx, *inst, "restart")
		if !ok {
			return &pb.RenameInstanceResponse{Ok: false, Message: fmt.Sprintf("display name saved but restart failed: %s", msg)}, nil
		}
		applied = append(applied, fmt.Sprintf("display name -> %q (restarted)", req.DisplayName))
	}

	if len(applied) == 0 {
		return &pb.RenameInstanceResponse{Ok: false, Message: "nothing to rename: both tmux_session_name and display_name were empty"}, nil
	}
	return &pb.RenameInstanceResponse{Ok: true, Message: fmt.Sprintf("renamed %s: %s", inst.Name, strings.Join(applied, ", "))}, nil
}
