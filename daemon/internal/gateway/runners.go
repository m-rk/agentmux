package gateway

import (
	"context"
	"net/http"
	"os"
	"os/user"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/runas"
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

// ProbeBackend is implemented by a Backend that keeps provider probe rows
// (AMUX-139). Optional, like RunnerBackend.
type ProbeBackend interface {
	ProviderHealth() []catalogue.ProviderHealth
}

// ProbeUnitPath is the systemd unit `daemon install` writes for the hourly
// probe; its User= names whose state a root daemon reads.
const ProbeUnitPath = "/etc/systemd/system/agentmuxd-probe.service"

// ProviderHealth reads this host's probe state: the daemon user's own, or
// on a root daemon the probe unit's user's. Unreadable state is no rows.
func (LocalBackend) ProviderHealth() []catalogue.ProviderHealth {
	home := runas.CurrentUserHome()
	if os.Geteuid() == 0 {
		home = probeUserHome(ProbeUnitPath)
		if home == "" {
			return []catalogue.ProviderHealth{}
		}
	}
	st, err := catalogue.LoadHealth(catalogue.HealthPath(home))
	if err != nil || st.Providers == nil {
		return []catalogue.ProviderHealth{}
	}
	return st.Providers
}

// probeUserHome is the home of the User= in a systemd unit file, or "".
func probeUserHome(unit string) string {
	b, err := os.ReadFile(unit)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "User="); ok && name != "" {
			if u, err := user.Lookup(name); err == nil {
				return u.HomeDir
			}
		}
	}
	return ""
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
	probes := []catalogue.ProviderHealth{}
	if pb, ok := s.backend.(ProbeBackend); ok {
		probes = pb.ProviderHealth()
	}
	s.reply(w, a, http.StatusOK, gatewayapi.RunnersResponse{Host: s.backend.Host(), Runners: rb.Runners(r.Context()), Probes: probes})
}
