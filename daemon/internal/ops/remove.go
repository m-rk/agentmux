package ops

import (
	"context"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// RemoveRequest removes one long-running instance while retaining its
// registry and service artifacts in the host's retired directory.
type RemoveRequest struct{ Instance string }

// RemoveResult reports the archived artifacts. The instance workdir remains.
type RemoveResult struct {
	Instance string `json:"instance"`
	Message  string `json:"message"`
}

// Remove stops and disables an instance through the privileged local daemon,
// then archives its registry and service artifacts. It deliberately leaves
// the instance's workdir and agent data untouched.
func (e Env) Remove(ctx context.Context, req RemoveRequest) (RemoveResult, error) {
	name := strings.TrimSpace(req.Instance)
	if err := provision.ValidateInstanceName(name); err != nil || strings.HasPrefix(name, ".") {
		return RemoveResult{}, Refuse(safesend.ReasonInvalid, "invalid instance name %q", name)
	}
	d, err := e.daemon()
	if err != nil {
		return RemoveResult{}, err
	}
	defer d.Close()
	resp, err := d.RetireInstance(ctx, &pb.RetireInstanceRequest{Instance: name, Archive: true})
	if err != nil {
		return RemoveResult{}, Refuse(safesend.ReasonFailed, "removing %s: %v", name, err)
	}
	if !resp.Ok {
		return RemoveResult{}, Refuse(safesend.ReasonNotFound, "%s", resp.Message)
	}
	return RemoveResult{Instance: name, Message: resp.Message}, nil
}
