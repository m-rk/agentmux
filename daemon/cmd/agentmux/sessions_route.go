package main

import (
	"errors"
	"net/http"
	"os"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// sessionRoute says where a `sessions` operation on one address runs: on this
// host (Remote is nil) or through another host's gateway.
type sessionRoute struct {
	Remote *gatewayclient.Client
}

// resolveRoute decides local vs remote for addrText. An address that doesn't
// parse routes local, so the local operation reports the error as it always
// has. The hosts file is read only for an address on another host, so a
// missing or broken hosts.yaml never affects local sessions. A host with no
// entry or no gateway is refused as not_local.
func resolveRoute(addrText, hostsPath, localHost string) (sessionRoute, error) {
	addr, err := address.Parse(addrText)
	if err != nil || addr.Host == localHost {
		return sessionRoute{}, nil
	}
	var hosts []hostsconfig.Host
	if hostsPath != "" {
		cfg, err := hostsconfig.Load(hostsPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return sessionRoute{}, ops.Refuse(safesend.ReasonFailed, "reading hosts file to reach host %q: %v", addr.Host, err)
		default:
			hosts = cfg.Hosts
		}
	}
	for _, h := range hosts {
		if address.Canonical(h.Name) != addr.Host {
			continue
		}
		if h.Gateway == "" {
			break
		}
		return sessionRoute{Remote: &gatewayclient.Client{BaseURL: h.Gateway, HTTP: &http.Client{}, Host: addr.Host}}, nil
	}
	return sessionRoute{}, ops.Refuse(safesend.ReasonNotLocal,
		"%s is not this host (%s); to reach it, add a host named %q with a gateway: URL to %s",
		addr.Host, localHost, addr.Host, hostsPathLabel(hostsPath))
}

func hostsPathLabel(p string) string {
	if p == "" {
		return "hosts.yaml"
	}
	return p
}
