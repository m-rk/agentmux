package ops

import (
	"context"
	"fmt"

	"github.com/m-rk/agentmux/daemon/internal/pb"
)

// RunTaskCheck invokes the daemon's fixed project check for a registered
// task. The request contains only an instance name; the daemon resolves its
// worktree, project and run user from the registry.
func (e Env) RunTaskCheck(ctx context.Context, instance string) (*pb.RunTaskCheckResponse, error) {
	d, err := e.dial()
	if err != nil {
		return nil, err
	}
	defer d.Close()
	resp, err := d.RunTaskCheck(ctx, &pb.RunTaskCheckRequest{Instance: instance})
	if err != nil {
		return nil, err
	}
	if !resp.Ok {
		return resp, fmt.Errorf("task check refused: %s", resp.Message)
	}
	return resp, nil
}
