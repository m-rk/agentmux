// Package gatewayclient calls another host's gateway (internal/gateway) over
// the wire contract in internal/gatewayapi. Errors come back as *ops.Error so
// callers treat a remote refusal like a local one. See docs/design/gateway.md
// (phase 4).
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

const (
	// QueryTimeout bounds list, status, threads and read; amp reads can be
	// slow.
	QueryTimeout = 60 * time.Second
	// CreateTimeout bounds create: a fetch, a worktree checkout and the
	// instance provisioning.
	CreateTimeout = 3 * time.Minute
	// SendMargin is added to a send's wait+confirm for the round trip and the
	// server's own checks.
	SendMargin = 30 * time.Second
	// MaxResponseBytes caps what is read from a gateway reply.
	MaxResponseBytes = 16 << 20
)

// Client talks to one host's gateway.
type Client struct {
	BaseURL string       // e.g. http://100.x.y.z:4288
	HTTP    *http.Client // nil means a client with no timeout of its own
	// Host names the gateway's host in error details.
	Host string
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{}
}

func (c *Client) where() string {
	if c.Host != "" {
		return fmt.Sprintf("gateway for host %q (%s)", c.Host, c.BaseURL)
	}
	return fmt.Sprintf("gateway %s", c.BaseURL)
}

// List returns the sessions the caller may see on the host.
func (c *Client) List(ctx context.Context) (gatewayapi.ListResponse, error) {
	var out gatewayapi.ListResponse
	err := c.call(ctx, gatewayapi.OpList, QueryTimeout, gatewayapi.ListRequest{}, &out)
	return out, err
}

func (c *Client) Status(ctx context.Context, req gatewayapi.StatusRequest) (gatewayapi.StatusResponse, error) {
	var out gatewayapi.StatusResponse
	err := c.call(ctx, gatewayapi.OpStatus, QueryTimeout, req, &out)
	return out, err
}

func (c *Client) Threads(ctx context.Context, req gatewayapi.ThreadsRequest) (gatewayapi.ThreadsResponse, error) {
	var out gatewayapi.ThreadsResponse
	err := c.call(ctx, gatewayapi.OpThreads, QueryTimeout, req, &out)
	return out, err
}

func (c *Client) Read(ctx context.Context, req gatewayapi.ReadRequest) (gatewayapi.ReadResponse, error) {
	var out gatewayapi.ReadResponse
	err := c.call(ctx, gatewayapi.OpRead, QueryTimeout, req, &out)
	return out, err
}

// Create starts a task session on the host and returns without waiting for
// it to be ready.
func (c *Client) Create(ctx context.Context, req gatewayapi.CreateRequest) (gatewayapi.CreateResponse, error) {
	var out gatewayapi.CreateResponse
	err := c.call(ctx, gatewayapi.OpCreate, CreateTimeout, req, &out)
	return out, err
}

// RunTimeout bounds a run: the server's own runTimeout plus the round trip.
const RunTimeout = 4 * time.Minute

// Run starts (or continues) an amp thread on the host.
func (c *Client) Run(ctx context.Context, req gatewayapi.RunRequest) (gatewayapi.RunResponse, error) {
	var out gatewayapi.RunResponse
	err := c.call(ctx, gatewayapi.OpRun, RunTimeout, req, &out)
	return out, err
}

// Retire ends one finished task session on the host.
func (c *Client) Retire(ctx context.Context, req gatewayapi.RetireRequest) (gatewayapi.RetireResponse, error) {
	var out gatewayapi.RetireResponse
	err := c.call(ctx, gatewayapi.OpRetire, QueryTimeout, req, &out)
	return out, err
}

// GC deletes the leftovers of retired sessions older than the host retention.
func (c *Client) GC(ctx context.Context, req gatewayapi.GCRequest) (gatewayapi.GCResponse, error) {
	var out gatewayapi.GCResponse
	err := c.call(ctx, gatewayapi.OpGC, QueryTimeout, req, &out)
	return out, err
}

// Send delivers a message. Like ops.Env.Send it never returns an error: a
// refusal, a transport failure or a bad reply all come back as a SendResult
// with OK false.
func (c *Client) Send(ctx context.Context, req gatewayapi.SendRequest) gatewayapi.SendResponse {
	timeout := time.Duration(req.WaitSeconds+req.ConfirmSeconds)*time.Second + SendMargin
	var out gatewayapi.SendResponse
	if err := c.call(ctx, gatewayapi.OpSend, timeout, req, &out); err != nil {
		e := ops.AsError(err)
		return gatewayapi.SendResponse{
			Address: req.Address, Reason: e.Reason, Retryable: e.Reason.Retryable(),
			Detail: e.Detail, Correlation: req.Correlation,
		}
	}
	return out
}

// call POSTs in as JSON to the op's path and decodes a 2xx reply into out.
func (c *Client) call(ctx context.Context, op string, timeout time.Duration, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return ops.Refuse(safesend.ReasonFailed, "encoding %s request: %v", op, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := strings.TrimRight(c.BaseURL, "/") + gatewayapi.Path(op)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ops.Refuse(safesend.ReasonFailed, "%s: %v", c.where(), err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ops.Refuse(safesend.ReasonFailed, "%s: %s timed out after %s", c.where(), op, timeout)
		}
		return ops.Refuse(safesend.ReasonFailed, "%s unreachable: %v", c.where(), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return ops.Refuse(safesend.ReasonFailed, "%s: reading %s reply: %v", c.where(), op, err)
	}
	if len(data) > MaxResponseBytes {
		return ops.Refuse(safesend.ReasonFailed, "%s: %s reply is over %d bytes", c.where(), op, MaxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var er gatewayapi.ErrorResponse
		if json.Unmarshal(data, &er) == nil && er.Error.Reason != "" {
			e := er.Error
			return &e
		}
		return ops.Refuse(safesend.ReasonFailed, "%s: %s returned HTTP %d with no error body", c.where(), op, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return ops.Refuse(safesend.ReasonFailed, "%s: bad %s reply: %v", c.where(), op, err)
	}
	return nil
}
