package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

// runnerProbe asks one host which runners it has.
type runnerProbe func(ctx context.Context, host string) (map[string]bool, error)

// checkHosts probes every host in parallel and returns one report each.
// A failed probe is recorded as an error report, never dropped, so the
// catalogue can say why an offering is unavailable there.
func checkHosts(ctx context.Context, hosts []string, now time.Time, local, remote runnerProbe) map[string]catalogue.HostReport {
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := make(map[string]catalogue.HostReport, len(hosts))
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			probe := remote
			if h == "local" || h == address.LocalHostName() {
				probe = local
			}
			rep := catalogue.HostReport{CheckedAt: now}
			runners, err := probe(ctx, h)
			if err != nil {
				rep.Err = err.Error()
			} else {
				rep.Runners = runners
			}
			mu.Lock()
			out[h] = rep
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return out
}

func localRunnerProbe(context.Context, string) (map[string]bool, error) {
	return catalogue.LocalRunners(), nil
}

// gatewayRunnerProbe asks a host's gateway (from hosts.yaml) for its runners.
func gatewayRunnerProbe(hostsPath string) runnerProbe {
	cfg, cfgErr := hostsconfig.Load(hostsPath)
	return func(ctx context.Context, host string) (map[string]bool, error) {
		if cfgErr != nil {
			return nil, errString("host list unreadable")
		}
		var base string
		for _, h := range cfg.Hosts {
			if h.Name == host {
				base = h.Gateway
			}
		}
		if base == "" {
			return nil, errString("host has no gateway in hosts.yaml")
		}
		c := &gatewayclient.Client{BaseURL: base, HTTP: &http.Client{}, Host: host}
		cctx, cancel := context.WithTimeout(ctx, gatewayclient.QueryTimeout)
		defer cancel()
		res, err := c.Runners(cctx)
		if err != nil {
			e := ops.AsError(err)
			return nil, errString("check failed: " + e.Reason)
		}
		out := map[string]bool{}
		for r, st := range res.Runners {
			out[r] = st.Present
		}
		return out, nil
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// readFuel fills in what this host can say about a fuel tank. Only the
// ChatGPT tank has a live source (Amp's providers list); a failure is
// reported on the tank, never hidden, and nothing from the output is kept.
func readFuel(ctx context.Context, cat *catalogue.Catalogue, ampPresent bool, read func(context.Context) (string, error)) {
	if !ampPresent {
		return
	}
	for i := range cat.Fuel {
		if cat.Fuel[i].Name != catalogue.FuelChatGPT {
			continue
		}
		v, err := read(ctx)
		if err != nil {
			cat.Fuel[i].Subscription = "unreadable: " + err.Error()
			continue
		}
		cat.Fuel[i].Subscription = v
	}
}
