package gatewayapi

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
}
