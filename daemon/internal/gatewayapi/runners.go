package gatewayapi

import "github.com/m-rk/agentmux/daemon/internal/catalogue"

// OpRunners reports which agent runners are installed on the host, so
// the model catalogue can say what is available there (AMUX-134).
const OpRunners = "runners"

// RunnerStatus is one runner's presence on a host.
type RunnerStatus struct {
	Present bool `json:"present"`
}

// RunnersResponse is the runners found on the host. Keys are runner
// names (claude-code, codex, amp, opencode, kilo).
type RunnersResponse struct {
	Host    string                  `json:"host"`
	Runners map[string]RunnerStatus `json:"runners"`
	// Probes are the host's own `agentmux models probe` rows (AMUX-139),
	// with Host "local". Always present from a gateway that knows about
	// probes, as [] when it has never probed; absent from an older one.
	Probes []catalogue.ProviderHealth `json:"probes"`
}
