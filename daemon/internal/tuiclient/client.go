// Package tuiclient wraps the AgentmuxDaemon gRPC client for use by the TUI:
// connecting over a Unix socket, listing/streaming instances, driving
// control actions, and piping a raw terminal through an Attach stream.
package tuiclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

type Client struct {
	Host string // label shown in the TUI; "local" for the unix-socket daemon
	conn *grpc.ClientConn
	api  pb.AgentmuxDaemonClient
}

func Dial(host, socketPath string) (*Client, error) {
	conn, err := grpc.NewClient("unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", socketPath, err)
	}
	return &Client{Host: host, conn: conn, api: pb.NewAgentmuxDaemonClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) ListInstances(ctx context.Context) ([]*pb.Instance, error) {
	resp, err := c.api.ListInstances(ctx, &pb.ListInstancesRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

// StreamEvents forwards daemon events onto the returned channel until ctx is
// canceled, closing the channel on exit.
func (c *Client) StreamEvents(ctx context.Context) (<-chan *pb.InstanceEvent, error) {
	stream, err := c.api.StreamEvents(ctx, &pb.StreamEventsRequest{})
	if err != nil {
		return nil, err
	}
	ch := make(chan *pb.InstanceEvent)
	go func() {
		defer close(ch)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (c *Client) Control(ctx context.Context, instance string, action pb.ControlAction) (*pb.ControlResponse, error) {
	return c.api.Control(ctx, &pb.ControlRequest{Instance: instance, Action: action})
}

// PeekAttach attaches to an instance and reports PTY output for dur without
// ever writing to stdin, so it's safe to use against a live session purely
// to verify the pipe works (used by smoke tests, not the TUI itself).
func (c *Client) PeekAttach(instance string, dur time.Duration, onData func(data []byte)) error {
	ctx, cancel := context.WithTimeout(context.Background(), dur+2*time.Second)
	defer cancel()

	stream, err := c.api.Attach(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_Attach{
		Attach: &pb.AttachRequest{Instance: instance},
	}}); err != nil {
		return err
	}

	done := time.After(dur)
	msgs := make(chan *pb.ServerMessage, 8)
	errc := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			msgs <- msg
		}
	}()

	for {
		select {
		case <-done:
			_ = stream.CloseSend()
			return nil
		case msg := <-msgs:
			switch p := msg.Payload.(type) {
			case *pb.ServerMessage_Stdout:
				onData(p.Stdout)
			case *pb.ServerMessage_Error:
				return fmt.Errorf("remote: %s", p.Error)
			}
		case err := <-errc:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// AttachInteractive puts the local terminal into raw mode and pipes it
// through the daemon's Attach stream for the given instance until the
// stream ends, ctx is canceled, or the user hits the detach key sequence
// (ctrl-\).
func (c *Client) AttachInteractive(ctx context.Context, instance string) error {
	stream, err := c.api.Attach(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_Attach{
		Attach: &pb.AttachRequest{Instance: instance},
	}}); err != nil {
		return err
	}

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("entering raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	if w, h, err := term.GetSize(fd); err == nil {
		_ = stream.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_Resize{
			Resize: &pb.Resize{Cols: uint32(w), Rows: uint32(h)},
		}})
	}

	errc := make(chan error, 2)

	// daemon -> local stdout
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				errc <- err
				return
			}
			switch p := msg.Payload.(type) {
			case *pb.ServerMessage_Stdout:
				os.Stdout.Write(p.Stdout)
			case *pb.ServerMessage_Error:
				errc <- fmt.Errorf("remote: %s", p.Error)
				return
			}
		}
	}()

	// local stdin -> daemon, watching for the ctrl-\ detach key (0x1c)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				for _, b := range buf[:n] {
					if b == 0x1c {
						errc <- nil
						return
					}
				}
				if sendErr := stream.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_Stdin{
					Stdin: append([]byte(nil), buf[:n]...),
				}}); sendErr != nil {
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

	err = <-errc
	_ = stream.CloseSend()
	return err
}
