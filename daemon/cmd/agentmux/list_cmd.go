package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// listRow is a flattened, JSON-friendly view of an instance plus the host it
// lives on — deliberately not pb.Instance itself, so the -json output shape
// stays stable even if the proto gains internal-only fields later.
type listRow struct {
	Host             string `json:"host"`
	Address          string `json:"address"` // <instance>@<host>; see internal/address
	Name             string `json:"name"`
	Agent            string `json:"agent"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Status           string `json:"status"`
	Workdir          string `json:"workdir"`
	TmuxSession      string `json:"tmux_session"`
	Pid              int64  `json:"pid"`
	LastActivityUnix int64  `json:"last_activity_unix"`
	StartedAtUnix    int64  `json:"started_at_unix"`
}

// runListCmd is `agentmux list`: a headless, scriptable counterpart to the
// TUI's instance table (name/agent/model/status/workdir per instance) for
// driving agentmux from scripts, cron jobs, or other agents without a TTY —
// the bare `agentmux` command requires one (see runTUI's tea.Program), so
// there was previously no way to answer "what's running, on what model"
// without either attaching interactively or reading registry files/systemd
// units by hand.
func runListCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "all", "host to list (a name from hosts.yaml, \"local\", or \"all\")")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON instead of a table")
	fs.Parse(args)

	hosts, err := loadHosts(*hostsPath, *socketPath)
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	if *host != "all" {
		var filtered []hostsconfig.Host
		for _, h := range hosts {
			if h.Name == *host {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) == 0 {
			log.Fatalf("list: host %q not found in %s", *host, *hostsPath)
		}
		hosts = filtered
	}

	rows, errs := collectRows(hosts)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			log.Fatalf("list: %v", err)
		}
	} else if len(rows) == 0 {
		fmt.Println("no instances found")
	} else {
		fmt.Printf("%-10s %-20s %-10s %-20s %-8s %s\n", "HOST", "NAME", "AGENT", "MODEL", "STATUS", "WORKDIR")
		for _, r := range rows {
			fmt.Printf("%-10s %-20s %-10s %-20s %-8s %s\n", r.Host, r.Name, r.Agent, r.Model, r.Status, r.Workdir)
		}
	}

	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "warning: "+e)
	}
	if len(errs) > 0 && len(rows) == 0 {
		os.Exit(1)
	}
}

// collectRows lists every instance on each host. A host that can't be
// reached or listed is reported in errs and skipped, so one down device
// doesn't hide the rest.
func collectRows(hosts []hostsconfig.Host) (rows []listRow, errs []string) {
	for _, h := range hosts {
		if h.Address == "" {
			r, err := gatewayRows(h)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", h.Name, err))
			}
			rows = append(rows, r...)
			continue
		}
		c, err := tuiclient.Dial(h.Name, h.Address)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name, err))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		instances, err := c.ListInstances(ctx)
		cancel()
		c.Close()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name, err))
			continue
		}
		for _, inst := range instances {
			rows = append(rows, listRow{
				Host:             h.Name,
				Address:          address.Address{Instance: inst.Name, Host: address.Canonical(h.Name)}.String(),
				Name:             inst.Name,
				Agent:            inst.Agent,
				Provider:         inst.Provider,
				Model:            inst.Model,
				Status:           statusLabel(inst.Status),
				Workdir:          inst.Workdir,
				TmuxSession:      inst.TmuxSession,
				Pid:              inst.Pid,
				LastActivityUnix: inst.LastActivityUnix,
				StartedAtUnix:    inst.StartedAtUnix,
			})
		}
	}
	return rows, errs
}

// gatewayRows lists a host that has only a gateway in hosts.yaml. The
// gateway shows only the sessions the caller's grant allows, and no tmux or
// process details.
func gatewayRows(h hostsconfig.Host) ([]listRow, error) {
	c := &gatewayclient.Client{BaseURL: h.Gateway, HTTP: &http.Client{}, Host: h.Name}
	ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.QueryTimeout)
	defer cancel()
	res, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]listRow, 0, len(res.Sessions))
	for _, s := range res.Sessions {
		rows = append(rows, listRow{
			Host: h.Name, Address: s.Address, Name: s.Name, Agent: s.Agent,
			Provider: s.Provider, Model: s.Model, Status: s.Status, Workdir: s.Workdir,
			LastActivityUnix: s.LastActivityUnix, StartedAtUnix: s.StartedAtUnix,
		})
	}
	return rows, nil
}
