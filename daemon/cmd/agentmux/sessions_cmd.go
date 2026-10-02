package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
)

// resolvedSession is `sessions resolve` output: the instance row plus the
// thread from the address, if one was given. The thread is passed through
// as given; checking that it exists needs the transcript reader (gateway
// phase 2).
type resolvedSession struct {
	listRow
	Thread string `json:"thread,omitempty"`
}

// runSessionsCmd is `agentmux sessions`: addressing for orchestrators. See
// docs/design/gateway.md (phase 1). Listing with addresses is `agentmux list
// -json`.
func runSessionsCmd(args []string) {
	if len(args) == 0 || args[0] != "resolve" {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions resolve [-json] <instance>@<host>[#<thread>]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("sessions resolve", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	fs.Parse(args[1:])
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions resolve [-json] <instance>@<host>[#<thread>]")
		os.Exit(2)
	}

	addr, err := address.Parse(fs.Arg(0))
	if err != nil {
		log.Fatalf("sessions resolve: %v", err)
	}
	hosts, err := loadHosts(*hostsPath, *socketPath)
	if err != nil {
		log.Fatalf("sessions resolve: %v", err)
	}
	var target []hostsconfig.Host
	var known []string
	for _, h := range hosts {
		known = append(known, address.Canonical(h.Name))
		if address.Canonical(h.Name) == addr.Host {
			target = append(target, h)
		}
	}
	if len(target) == 0 {
		log.Fatalf("sessions resolve: no host %q (known: %s)", addr.Host, strings.Join(known, ", "))
	}

	rows, errs := collectRows(target)
	if len(errs) > 0 {
		log.Fatalf("sessions resolve: %s", strings.Join(errs, "; "))
	}
	for _, r := range rows {
		if r.Name != addr.Instance {
			continue
		}
		out := resolvedSession{listRow: r, Thread: addr.Thread}
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				log.Fatalf("sessions resolve: %v", err)
			}
			return
		}
		fmt.Printf("address  %s\nhost     %s\nagent    %s\nstatus   %s\nworkdir  %s\n", addr, r.Host, r.Agent, r.Status, r.Workdir)
		if addr.Thread != "" {
			fmt.Printf("thread   %s\n", addr.Thread)
		}
		return
	}
	log.Fatalf("sessions resolve: no instance %q on host %q", addr.Instance, addr.Host)
}
