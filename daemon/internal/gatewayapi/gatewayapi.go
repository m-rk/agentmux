// Package gatewayapi is the wire contract of the per-host gateway that lets
// an orchestrator on another host list, read, check and message this host's
// sessions over the tailnet. Both the server (internal/gateway) and the client
// (internal/gatewayclient) use these types. See docs/design/gateway.md
// (phase 4).
//
// Transport: HTTP on the host's tailnet address. Every operation is a POST of
// a JSON request body to Path(op), answered with a JSON body. A refusal is
// HTTP 200 with ok-shaped results where the operation has them (SendResponse),
// and otherwise a non-2xx status with an ErrorResponse body.
package gatewayapi

import (
	"net/http"

	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// DefaultPort is the gateway's TCP port; agentmuxd itself uses 4287.
const DefaultPort = 4288

// Operations, as named in the app capability's "ops" lists.
const (
	OpList    = "list"
	OpStatus  = "status"
	OpThreads = "threads"
	OpRead    = "read"
	OpSend    = "send"
	OpCreate  = "create"
	OpEvents  = "events" // reserved for phase 5
)

// Ops lists every operation a capability may name.
var Ops = []string{OpList, OpStatus, OpThreads, OpRead, OpSend, OpCreate, OpEvents}

// Path is the URL path for op, e.g. /v1/send.
func Path(op string) string { return "/v1/" + op }

// Grant is one entry of the app capability's value array in the tailnet
// policy grant:
//
//	"<domain>/cap/agentmux-gateway": [
//	  {"ops": ["list", "read", "status", "threads"], "sessions": ["*@*"]},
//	  {"ops": ["send"], "sessions": ["web*@*"]}
//	]
//
// A request is allowed when any Grant lists its op and has a session glob
// (path.Match syntax) matching the target address without its #thread. The
// list op has no single target; it is allowed by any Grant naming it, and
// its results are filtered to the sessions that Grant's globs match.
type Grant struct {
	Ops      []string `json:"ops"`
	Sessions []string `json:"sessions"`
}

type ListRequest struct{}

type ListResponse struct {
	Host     string        `json:"host"`
	Sessions []ops.Session `json:"sessions"`
}

type StatusRequest struct {
	Address string `json:"address"`
}

type StatusResponse = ops.StatusResult

type ThreadsRequest struct {
	Address string `json:"address"`
}

type ThreadsResponse struct {
	Threads []transcript.Thread `json:"threads"`
}

type ReadRequest struct {
	Address string `json:"address"`
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type ReadResponse = transcript.Page

// SendRequest has no principal: the gateway sets ops.SendRequest.By from the
// caller's tailnet identity.
type SendRequest struct {
	Address        string `json:"address"`
	Text           string `json:"text"`
	Via            string `json:"via"`
	From           string `json:"from,omitempty"`
	Correlation    string `json:"correlation,omitempty"`
	WaitSeconds    int    `json:"wait_seconds,omitempty"`
	ConfirmSeconds int    `json:"confirm_seconds,omitempty"`
}

type SendResponse = ops.SendResult

// CreateRequest starts a task session on the host: a Git worktree on Branch
// plus an instance in it, configured like Template. Instance is the new
// session, and the grant is checked against <instance>@<host>. See
// ops.CreateRequest.
type CreateRequest struct {
	Template   string   `json:"template"`
	Instance   string   `json:"instance"`
	Branch     string   `json:"branch"`
	Base       string   `json:"base,omitempty"`
	Worktree   string   `json:"worktree,omitempty"`
	AllowFiles []string `json:"allow_files,omitempty"`
}

// CreateResponse is the session plus branch and created; a refusal is a
// non-2xx ErrorResponse.
type CreateResponse = ops.CreateResult

// ErrorResponse is the body of every non-2xx reply.
type ErrorResponse struct {
	Error ops.Error `json:"error"`
}

// HTTPStatus maps a refusal reason to the status the server replies with.
func HTTPStatus(r safesend.Reason) int {
	switch r {
	case safesend.ReasonInvalid:
		return http.StatusBadRequest
	case safesend.ReasonForbidden:
		return http.StatusForbidden
	case safesend.ReasonNotFound:
		return http.StatusNotFound
	case safesend.ReasonRateLimited:
		return http.StatusTooManyRequests
	case safesend.ReasonNotLocal:
		return http.StatusMisdirectedRequest
	case safesend.ReasonUnsupported:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}
