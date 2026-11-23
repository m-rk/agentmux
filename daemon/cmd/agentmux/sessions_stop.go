package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

const sessionsStopUsage = "usage: agentmux sessions stop -by PRINCIPAL [-via relayed|dispatched|sent] [-from REF] [-correlation ID] [-json] [-dry-run] [-socket PATH] [-hosts PATH] <instance>@<host>[#<thread>]"

// runSessionsStop is `agentmux sessions stop`: cancel the active run of an
// instance, locally or through its host's gateway, and leave the instance,
// worktree and branch alone (see ops.Env.Stop). An idle instance is a
// no-op that still succeeds. Exit 0 stopped or idle, 1 refused or failed,
// 2 usage.
//
// Task sessions refuse (see liveguard), as for send, so a task instance
// cannot stop another session's work.
func runSessionsStop(args []string) {
	fs := flag.NewFlagSet("sessions stop", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON (also on refusal)")
	dryRun := fs.Bool("dry-run", false, "say what would be stopped without stopping it")
	via := fs.String("via", "relayed", "provenance verb: "+strings.Join(safesend.Vias, ", "))
	by := fs.String("by", "", "principal recorded in the audit log and the run log (required locally)")
	from := fs.String("from", "", "optional provenance source, e.g. a task id")
	correlation := fs.String("correlation", "", "opaque id recorded in the audit log")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, sessionsStopUsage)
		os.Exit(2)
	}
	addrText := fs.Arg(0)
	if !*dryRun {
		if err := liveguard.Check(); err != nil {
			failStop(*jsonOut, addrText, safesend.ReasonForbidden, err.Error())
		}
	}
	req := ops.StopRequest{Address: addrText, Via: *via, By: *by, From: *from, Correlation: *correlation, DryRun: *dryRun}
	var res ops.StopResult
	route, rerr := resolveRoute(req.Address, *hostsPath, address.LocalHostName())
	switch {
	case rerr != nil:
		e := ops.AsError(rerr)
		failStop(*jsonOut, req.Address, e.Reason, e.Detail)
	case route.Remote != nil:
		if *by != "" {
			fmt.Fprintln(os.Stderr, "note: -by ignored for a remote session; the gateway sets the principal from your tailnet identity")
		}
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.StopTimeout+time.Minute)
		defer cancel()
		var err error
		res, err = route.Remote.Stop(ctx, gatewayapi.StopRequest{Address: req.Address, Via: req.Via, From: req.From, Correlation: req.Correlation, DryRun: req.DryRun})
		if err != nil {
			e := ops.AsError(err)
			failStop(*jsonOut, req.Address, e.Reason, e.Detail)
		}
	default:
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.StopTimeout)
		defer cancel()
		var err error
		res, err = ops.Env{SocketPath: *socketPath}.Stop(ctx, req)
		if err != nil {
			e := ops.AsError(err)
			failStop(*jsonOut, req.Address, e.Reason, e.Detail)
		}
	}
	if *jsonOut {
		writeJSON(res)
		return
	}
	target := res.Address
	if res.Thread != "" && !strings.Contains(target, "#") {
		target += "#" + res.Thread
	}
	switch {
	case res.DryRun && len(res.Plan) > 0:
		fmt.Printf("would stop %s (%s):\n", target, res.Agent)
		for _, step := range res.Plan {
			fmt.Printf("  - %s\n", step)
		}
	case res.DryRun:
		fmt.Printf("nothing to stop on %s (%s)\n", target, res.State)
	case res.Stopped:
		fmt.Printf("stopped %s (%s); state %s\n", target, res.Agent, res.State)
	default:
		fmt.Printf("nothing to stop on %s (%s)\n", target, res.State)
	}
}

// failStop reports a refusal in the requested shape and exits 1.
func failStop(jsonOut bool, addr string, reason safesend.Reason, detail string) {
	if jsonOut {
		writeJSON(map[string]any{"ok": false, "address": addr, "reason": reason, "detail": detail})
	} else {
		fmt.Fprintf(os.Stderr, "not stopped %s: %s: %s\n", addr, reason, detail)
	}
	os.Exit(1)
}
