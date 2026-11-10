package gateway

import (
	"context"
	"net/http"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// RunnerBackend is implemented by a Backend that can say which runners
// are installed. Optional, like WarningBackend.
type RunnerBackend interface {
	Runners(ctx context.Context) map[string]gatewayapi.RunnerStatus
}

// Runners reports which runners are on this host's PATH.
func (LocalBackend) Runners(_ context.Context) map[string]gatewayapi.RunnerStatus {
	out := map[string]gatewayapi.RunnerStatus{}
	for r, present := range catalogue.LocalRunners() {
		out[r] = gatewayapi.RunnerStatus{Present: present}
	}
	return out
}

// runnersOp serves OpRunners: host-wide and read-only, so it is checked
// with opAllowed like versions. The reply names runners only.
func (s *Server) runnersOp(w http.ResponseWriter, r *http.Request, a *access, id Identity) {
	if !s.other.allow(a.principal) {
		s.rateLimited(w, a, s.other.limit)
		return
	}
	a.target = "-"
	if !opAllowed(id.Grants, gatewayapi.OpRunners) {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, gatewayapi.OpRunners+" is not permitted")
		return
	}
	rb, ok := s.backend.(RunnerBackend)
	if !ok {
		s.refuse(w, a, http.StatusNotImplemented, safesend.ReasonForbidden, "this host cannot report runners")
		return
	}
	s.reply(w, a, http.StatusOK, gatewayapi.RunnersResponse{Host: s.backend.Host(), Runners: rb.Runners(r.Context())})
}
