// Package daemonserver implements the AgentmuxDaemon gRPC service: it
// reports instance status from the discovery package, attaches clients to a
// tmux session's PTY, and drives systemctl for lifecycle control.
package daemonserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"reflect"
	"time"

	"github.com/creack/pty"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

const pollInterval = 2 * time.Second

type Server struct {
	pb.UnimplementedAgentmuxDaemonServer
}

func New() *Server {
	return &Server{}
}

func (s *Server) ListInstances(ctx context.Context, _ *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error) {
	instances, err := discovery.List()
	if err != nil {
		return nil, err
	}
	resp := &pb.ListInstancesResponse{}
	for _, inst := range instances {
		resp.Instances = append(resp.Instances, toProto(inst))
	}
	return resp, nil
}

func (s *Server) StreamEvents(_ *pb.StreamEventsRequest, stream pb.AgentmuxDaemon_StreamEventsServer) error {
	ctx := stream.Context()
	known := map[string]discovery.Instance{}

	send := func() error {
		current, err := discovery.List()
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, inst := range current {
			seen[inst.Name] = true
			if prev, ok := known[inst.Name]; !ok || !sameState(prev, inst) {
				known[inst.Name] = inst
				if err := stream.Send(&pb.InstanceEvent{Type: pb.EventType_EVENT_UPDATED, Instance: toProto(inst)}); err != nil {
					return err
				}
			}
		}
		for name, inst := range known {
			if !seen[name] {
				delete(known, name)
				if err := stream.Send(&pb.InstanceEvent{Type: pb.EventType_EVENT_REMOVED, Instance: toProto(inst)}); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := send(); err != nil {
		return err
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := send(); err != nil {
				return err
			}
		}
	}
}

func (s *Server) Control(ctx context.Context, req *pb.ControlRequest) (*pb.ControlResponse, error) {
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
		return &pb.ControlResponse{Ok: false, Message: fmt.Sprintf("unknown instance %q", req.Instance)}, nil
	}

	var action string
	switch req.Action {
	case pb.ControlAction_CONTROL_START:
		action = "start"
	case pb.ControlAction_CONTROL_STOP:
		action = "stop"
	case pb.ControlAction_CONTROL_RESTART:
		action = "restart"
	default:
		return &pb.ControlResponse{Ok: false, Message: "unknown action"}, nil
	}

	out, err := exec.CommandContext(ctx, "systemctl", action, inst.ServiceName).CombinedOutput()
	if err != nil {
		return &pb.ControlResponse{Ok: false, Message: fmt.Sprintf("%s %s: %v: %s", action, inst.ServiceName, err, out)}, nil
	}
	return &pb.ControlResponse{Ok: true, Message: fmt.Sprintf("%s %s ok", action, inst.ServiceName)}, nil
}

func (s *Server) Attach(stream pb.AgentmuxDaemon_AttachServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	attach := first.GetAttach()
	if attach == nil {
		return errors.New("first message must be an AttachRequest")
	}

	instances, err := discovery.List()
	if err != nil {
		return err
	}
	var session, socket string
	found := false
	for _, inst := range instances {
		if inst.Name == attach.Instance {
			session, socket = inst.TmuxSession, inst.TmuxSocket
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown instance %q", attach.Instance)
	}
	if socket == "" {
		return fmt.Errorf("instance %q has no live tmux session to attach to", attach.Instance)
	}

	cmd := exec.Command("tmux", "-S", socket, "attach-session", "-t", session)
	// agentmuxd usually runs under systemd with no controlling terminal, so
	// TERM is unset; tmux refuses to attach without one.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("attaching to tmux session %q: %w", session, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = f.Close()
	}()

	errc := make(chan error, 2)

	// PTY -> client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: append([]byte(nil), buf[:n]...)}}); sendErr != nil {
					errc <- sendErr
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					errc <- err
				} else {
					errc <- nil
				}
				return
			}
		}
	}()

	// client -> PTY
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			switch p := msg.Payload.(type) {
			case *pb.ClientMessage_Stdin:
				if _, err := f.Write(p.Stdin); err != nil {
					errc <- err
					return
				}
			case *pb.ClientMessage_Resize:
				_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(p.Resize.Cols), Rows: uint16(p.Resize.Rows)})
			}
		}
	}()

	err = <-errc
	if err != nil && err != io.EOF {
		log.Printf("attach %s: %v", attach.Instance, err)
	}
	return nil
}

func toProto(inst discovery.Instance) *pb.Instance {
	var status pb.Status
	switch inst.Status {
	case discovery.StatusRunning:
		status = pb.Status_STATUS_RUNNING
	case discovery.StatusIdle:
		status = pb.Status_STATUS_IDLE
	case discovery.StatusDead:
		status = pb.Status_STATUS_DEAD
	default:
		status = pb.Status_STATUS_UNKNOWN
	}
	return &pb.Instance{
		Name:             inst.Name,
		Agent:            inst.Agent,
		Provider:         inst.Provider,
		Model:            inst.Model,
		Workdir:          inst.Workdir,
		TmuxSession:      inst.TmuxSession,
		Status:           status,
		Pid:              inst.PID,
		LastActivityUnix: unixOrZero(inst.LastActivity),
		StartedAtUnix:    unixOrZero(inst.StartedAt),
	}
}

// unixOrZero avoids reporting the zero time.Time's Unix() value (a large
// negative number, year 1) as a real timestamp for instances with no live
// tmux session.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func sameState(a, b discovery.Instance) bool {
	a.LastActivity, b.LastActivity = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
