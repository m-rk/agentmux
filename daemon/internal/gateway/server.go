// Package gateway is the per-host HTTP service that lets an orchestrator on
// another host list, check, read and message this host's agent sessions. It
// serves the wire contract in internal/gatewayapi and calls the operations in
// internal/ops. See docs/gateway.md and docs/design/gateway.md (phase 4).
//
// Every request is attributed to a tailnet node by `tailscale whois` on its
// remote address, and authorized by the app capability the tailnet policy
// grants that node (gatewayapi.Grant). Anything short of a positive answer
// refuses the request.
//
// Refusals come in two shapes. A problem with the request as a whole (unknown
// caller, wrong method, bad route, unreadable or oversized body, events not
// built) is a non-2xx gatewayapi.ErrorResponse. For send, a request that
// parsed and was then refused (forbidden, rate_limited, or any reason from
// ops) is HTTP 200 with the ops.SendResult, ok false and a reason, the same
// shape a delivered send has. Every other operation, create included, refuses with a non-2xx
// ErrorResponse whose status is gatewayapi.HTTPStatus(reason).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

const (
	// MaxBodyBytes bounds a request body; a send's text is itself capped
	// well under this by safesend.MaxTextBytes.
	MaxBodyBytes = 128 << 10

	maxWait        = 10 * time.Minute
	maxConfirm     = 60 * time.Second
	defaultConfirm = 15 * time.Second

	// WhoisTTL is how long a node's identity and grants are remembered.
	WhoisTTL = 30 * time.Second
)

// Config builds a Server. Backend and Whois are required.
type Config struct {
	Backend Backend
	Whois   Whois
	// Logger receives the access log, one line per request. Nil discards it.
	Logger *log.Logger
	// SendLimit and OtherLimit are per principal; the zero Config uses the
	// Default limits. A Limit with PerMinute < 0 turns that limit off.
	SendLimit, OtherLimit *Limit
	WhoisTTL              time.Duration // 0 means WhoisTTL
	Now                   func() time.Time
}

// Server is the gateway's http.Handler.
type Server struct {
	backend Backend
	whois   *cachedWhois
	logger  *log.Logger
	send    *limiter
	other   *limiter
	now     func() time.Time
}

// New builds a Server from cfg.
func New(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ttl := cfg.WhoisTTL
	if ttl == 0 {
		ttl = WhoisTTL
	}
	sendLimit, otherLimit := DefaultSendLimit, DefaultOtherLimit
	if cfg.SendLimit != nil {
		sendLimit = *cfg.SendLimit
	}
	if cfg.OtherLimit != nil {
		otherLimit = *cfg.OtherLimit
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		backend: cfg.Backend,
		whois:   newCachedWhois(cfg.Whois, ttl, now),
		logger:  logger,
		send:    newLimiter(sendLimit, now),
		other:   newLimiter(otherLimit, now),
		now:     now,
	}
}

// access is what one request logs. It never holds message text or
// transcript content.
type access struct {
	principal string
	op        string
	target    string
	reason    string // "ok" or a safesend.Reason
	status    int
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	a := &access{principal: "-", reason: "ok", status: http.StatusOK}
	defer func() {
		s.logger.Printf("gateway: principal=%s op=%s target=%s status=%d result=%s remote=%s took=%s",
			a.principal, orDash(a.op), orDash(a.target), a.status, a.reason, r.RemoteAddr, s.now().Sub(start).Round(time.Millisecond))
	}()
	s.serve(w, r, a)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, a *access) {
	op, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok || !knownOp(op) {
		s.refuse(w, a, http.StatusNotFound, safesend.ReasonNotFound, "no such route")
		return
	}
	a.op = op
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.refuse(w, a, http.StatusMethodNotAllowed, safesend.ReasonInvalid, "use POST")
		return
	}

	id, err := s.whois.lookup(r.Context(), r.RemoteAddr)
	if err != nil || id.Principal() == "" {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, "caller is not a known tailnet node")
		return
	}
	a.principal = id.Principal()
	if len(id.Grants) == 0 {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, "caller has no agentmux gateway capability")
		return
	}
	if op == gatewayapi.OpEvents {
		s.refuse(w, a, http.StatusNotImplemented, safesend.ReasonUnsupported, "events are not implemented")
		return
	}

	body := http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	switch op {
	case gatewayapi.OpList:
		var req gatewayapi.ListRequest
		if !s.decode(w, a, body, &req, true) {
			return
		}
		s.list(w, r, a, id)
	case gatewayapi.OpStatus:
		var req gatewayapi.StatusRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Address)
		s.perSession(w, a, id, op, req.Address, func(ctx context.Context) (any, error) {
			return s.backend.Status(ctx, req.Address)
		}, r)
	case gatewayapi.OpThreads:
		var req gatewayapi.ThreadsRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Address)
		s.perSession(w, a, id, op, req.Address, func(ctx context.Context) (any, error) {
			threads, err := s.backend.Threads(ctx, req.Address)
			if threads == nil {
				threads = []transcript.Thread{}
			}
			return gatewayapi.ThreadsResponse{Threads: threads}, err
		}, r)
	case gatewayapi.OpRead:
		var req gatewayapi.ReadRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Address)
		s.perSession(w, a, id, op, req.Address, func(ctx context.Context) (any, error) {
			return s.backend.Read(ctx, req.Address, req.Cursor, req.Limit)
		}, r)
	case gatewayapi.OpCreate:
		var req gatewayapi.CreateRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Instance + "@" + s.backend.Host())
		s.createOp(w, r, a, id, req)
	case gatewayapi.OpSend:
		var req gatewayapi.SendRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Address)
		s.sendOp(w, r, a, id, req)
	case gatewayapi.OpRun:
		var req gatewayapi.RunRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Address)
		s.runOp(w, r, a, id, req)
	case gatewayapi.OpRetire:
		var req gatewayapi.RetireRequest
		if !s.decode(w, a, body, &req, false) {
			return
		}
		a.target = clip(req.Address)
		s.perSession(w, a, id, op, req.Address, func(ctx context.Context) (any, error) {
			return s.backend.Retire(ctx, ops.RetireRequest{Address: req.Address, DryRun: req.DryRun, RequireMerged: req.RequireMerged})
		}, r)
	case gatewayapi.OpGC:
		var req gatewayapi.GCRequest
		if !s.decode(w, a, body, &req, true) {
			return
		}
		a.target = "-"
		if !opAllowed(id.Grants, op) {
			s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, "gc is not permitted")
			return
		}
		out, err := s.backend.GC(r.Context(), ops.GCRequest{DryRun: req.DryRun})
		if err != nil {
			e := ops.AsError(err)
			s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
			return
		}
		s.reply(w, a, http.StatusOK, out)
	case gatewayapi.OpShipPublish, gatewayapi.OpVersions, gatewayapi.OpSelfUpdateLog:
		s.selfUpdateOp(w, r, a, id, op, body)
	}
}

// perSession is the shared path of the operations that name one session:
// rate limit, parse the address, check the grants, run, reply.
func (s *Server) perSession(w http.ResponseWriter, a *access, id Identity, op, addrText string, run func(context.Context) (any, error), r *http.Request) {
	if !s.other.allow(a.principal) {
		s.rateLimited(w, a, s.other.limit)
		return
	}
	addr, err := address.Parse(addrText)
	if err != nil {
		s.refuse(w, a, http.StatusBadRequest, safesend.ReasonInvalid, err.Error())
		return
	}
	if !sessionAllowed(id.Grants, op, addr.Session().String()) {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, fmt.Sprintf("%s on %s is not permitted", op, addr.Session()))
		return
	}
	out, err := run(r.Context())
	if err != nil {
		e := ops.AsError(err)
		s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
		return
	}
	s.reply(w, a, http.StatusOK, out)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, a *access, id Identity) {
	if !s.other.allow(a.principal) {
		s.rateLimited(w, a, s.other.limit)
		return
	}
	var globs []string
	for _, g := range id.Grants {
		if grantHasOp(g, gatewayapi.OpList) {
			globs = append(globs, g.Sessions...)
		}
	}
	if len(globs) == 0 {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, "list is not permitted")
		return
	}
	all, err := s.backend.List(r.Context())
	if err != nil {
		e := ops.AsError(err)
		s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
		return
	}
	resp := gatewayapi.ListResponse{Host: s.backend.Host(), Sessions: []ops.Session{}}
	for _, sess := range all {
		if matchAny(globs, sess.Address) {
			resp.Sessions = append(resp.Sessions, sess)
		}
	}
	s.reply(w, a, http.StatusOK, resp)
}

func (s *Server) sendOp(w http.ResponseWriter, r *http.Request, a *access, id Identity, req gatewayapi.SendRequest) {
	refuse := func(reason safesend.Reason, detail string) {
		a.reason = string(reason)
		s.reply(w, a, http.StatusOK, ops.SendResult{
			Address: req.Address, Reason: reason, Retryable: reason.Retryable(),
			Detail: detail, Correlation: req.Correlation,
		})
	}
	if !s.send.allow(a.principal) {
		refuse(safesend.ReasonRateLimited, fmt.Sprintf("over %g sends a minute", s.send.limit.PerMinute))
		return
	}
	addr, err := address.Parse(req.Address)
	if err != nil {
		refuse(safesend.ReasonInvalid, err.Error())
		return
	}
	if !sessionAllowed(id.Grants, gatewayapi.OpSend, addr.Session().String()) {
		refuse(safesend.ReasonForbidden, fmt.Sprintf("send to %s is not permitted", addr.Session()))
		return
	}
	res := s.backend.Send(r.Context(), ops.SendRequest{
		Address:     req.Address,
		Text:        req.Text,
		Via:         req.Via,
		By:          a.principal, // never from the request
		From:        req.From,
		Correlation: req.Correlation,
		Wait:        clampSeconds(req.WaitSeconds, 0, maxWait),
		Confirm:     clampSeconds(req.ConfirmSeconds, defaultConfirm, maxConfirm),
	})
	if !res.OK {
		a.reason = string(res.Reason)
	}
	s.reply(w, a, http.StatusOK, res)
}

// createOp starts a task session. The grant is checked against the new
// instance's address on this host. It shares the send rate bucket, since it
// starts an agent that will act on text it is later sent. Refusals are
// ErrorResponses like every op but send.
func (s *Server) createOp(w http.ResponseWriter, r *http.Request, a *access, id Identity, req gatewayapi.CreateRequest) {
	if !s.send.allow(a.principal) {
		s.rateLimited(w, a, s.send.limit)
		return
	}
	addr, err := address.Parse(req.Instance + "@" + s.backend.Host())
	if err != nil {
		s.refuse(w, a, http.StatusBadRequest, safesend.ReasonInvalid, err.Error())
		return
	}
	if !sessionAllowed(id.Grants, gatewayapi.OpCreate, addr.Session().String()) {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, fmt.Sprintf("create of %s is not permitted", addr.Session()))
		return
	}
	res, err := s.backend.Create(r.Context(), ops.CreateRequest{
		Template: req.Template, Instance: req.Instance, Branch: req.Branch,
		Base: req.Base, Worktree: req.Worktree, AllowFiles: req.AllowFiles,
		DryRun: req.DryRun,
	})
	if err != nil {
		e := ops.AsError(err)
		s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
		return
	}
	s.reply(w, a, http.StatusOK, res)
}

// runOp starts (or continues) an amp thread. The grant is checked against
// the session without its #thread: starting needs "run" on the instance,
// continuing a thread needs it too — a thread suffix never widens access.
// It shares the send rate bucket, since it starts an agent that will act
// on text it is given. Refusals are ErrorResponses like every op but send.
func (s *Server) runOp(w http.ResponseWriter, r *http.Request, a *access, id Identity, req gatewayapi.RunRequest) {
	if !s.send.allow(a.principal) {
		s.rateLimited(w, a, s.send.limit)
		return
	}
	addr, err := address.Parse(req.Address)
	if err != nil {
		s.refuse(w, a, http.StatusBadRequest, safesend.ReasonInvalid, err.Error())
		return
	}
	if !sessionAllowed(id.Grants, gatewayapi.OpRun, addr.Session().String()) {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, fmt.Sprintf("run on %s is not permitted", addr.Session()))
		return
	}
	res, err := s.backend.Run(r.Context(), ops.RunRequest{Address: req.Address, Text: req.Text, Title: req.Title, Labels: req.Labels, Mode: req.Mode, DryRun: req.DryRun, Template: req.Template})
	if err != nil {
		e := ops.AsError(err)
		s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
		return
	}
	s.reply(w, a, http.StatusOK, res)
}

// clampSeconds turns a request's seconds into a duration in [0, limit];
// zero or less means def.
func clampSeconds(sec int, def, limit time.Duration) time.Duration {
	if sec <= 0 {
		return def
	}
	if d := time.Duration(sec) * time.Second; d < limit {
		return d
	}
	return limit
}

// decode reads the one JSON object of the body into v, rejecting unknown
// fields and trailing data. emptyOK lets an empty body stand for {}.
func (s *Server) decode(w http.ResponseWriter, a *access, body io.Reader, v any, emptyOK bool) bool {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) && emptyOK {
		return true
	}
	if err == nil {
		if _, terr := dec.Token(); !errors.Is(terr, io.EOF) {
			err = errors.New("unexpected data after the JSON object")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.refuse(w, a, http.StatusRequestEntityTooLarge, safesend.ReasonInvalid, fmt.Sprintf("request body is over %d bytes", MaxBodyBytes))
			return false
		}
		s.refuse(w, a, http.StatusBadRequest, safesend.ReasonInvalid, "request body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) rateLimited(w http.ResponseWriter, a *access, l Limit) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Max(1, math.Ceil(60/l.PerMinute)))))
	s.refuse(w, a, http.StatusTooManyRequests, safesend.ReasonRateLimited, "too many requests")
}

// refuse replies with an ErrorResponse.
func (s *Server) refuse(w http.ResponseWriter, a *access, status int, reason safesend.Reason, detail string) {
	a.reason = string(reason)
	s.reply(w, a, status, gatewayapi.ErrorResponse{Error: ops.Error{Reason: reason, Detail: detail}})
}

func (s *Server) reply(w http.ResponseWriter, a *access, status int, v any) {
	a.status = status
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Printf("gateway: writing reply: %v", err)
	}
}

// clip bounds a client-supplied string for the log and makes it one safe
// token; addresses are short, and %q keeps control characters out.
func clip(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	return strconv.Quote(s)
}
