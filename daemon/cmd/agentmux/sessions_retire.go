package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

// runSessionsRetire is `agentmux sessions retire`: end one finished task
// session on its host (local or through its gateway), after its work is
// done and merged. See the retire package and ops.Env.Retire. Exit 0
// retired, 1 refused or failed, 2 usage.
func runSessionsRetire(args []string) {
	fs := flag.NewFlagSet("sessions retire", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON (also on refusal)")
	dryRun := fs.Bool("dry-run", false, "list what would go without changing anything")
	requireMerged := fs.Bool("require-merged", false, "refuse the retire when a branch isn't provably merged, instead of retiring with the branch kept")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions retire [-json] [-dry-run] [-require-merged] [-socket PATH] [-hosts PATH] <instance>@<host>")
		os.Exit(2)
	}
	req := ops.RetireRequest{Address: fs.Arg(0), DryRun: *dryRun, RequireMerged: *requireMerged}
	var res ops.RetireResult
	route, rerr := resolveRoute(req.Address, *hostsPath, address.LocalHostName())
	switch {
	case rerr != nil:
		e := ops.AsError(rerr)
		failRetire(*jsonOut, req.Address, e.Reason, e.Detail)
	case route.Remote != nil:
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.QueryTimeout+time.Minute)
		defer cancel()
		var rerr error
		res, rerr = route.Remote.Retire(ctx, gatewayapi.RetireRequest{Address: req.Address, DryRun: req.DryRun, RequireMerged: req.RequireMerged})
		if rerr != nil {
			e := ops.AsError(rerr)
			failRetire(*jsonOut, req.Address, e.Reason, e.Detail)
		}
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var rerr error
		res, rerr = ops.Env{SocketPath: *socketPath}.Retire(ctx, req)
		if rerr != nil {
			e := ops.AsError(rerr)
			failRetire(*jsonOut, req.Address, e.Reason, e.Detail)
		}
	}
	if *jsonOut {
		writeJSON(map[string]any{"ok": true, "retire": res})
		return
	}
	if res.DryRun {
		fmt.Printf("would retire %s (%s):\n", res.Address, res.Agent)
		for _, step := range res.Plan {
			fmt.Printf("  - %s\n", step)
		}
		return
	}
	fmt.Printf("retired %s (%s)\n", res.Address, res.Agent)
	for _, thread := range res.AmpThreads {
		fmt.Printf("archived amp thread %s (deleted by gc after retention)\n", thread)
	}
	if len(res.OpencodeSessions) > 0 {
		fmt.Printf("%d stored opencode sessions recorded for gc\n", len(res.OpencodeSessions))
	}
	if res.WorktreeKept != "" {
		fmt.Printf("kept worktree %s: %s\n", res.Workdir, res.WorktreeKept)
	} else if res.Workdir != "" {
		fmt.Printf("removed worktree %s\n", res.Workdir)
	}
	if len(res.Branches) > 0 {
		for _, f := range res.Branches {
			if f.Deleted {
				fmt.Printf("deleted branch %s (upstream contains it)\n", f.Branch)
			} else if f.Kept != "" {
				fmt.Printf("kept branch %s: %s\n", f.Branch, f.Kept)
			} else {
				fmt.Printf("kept branch %s\n", f.Branch)
			}
		}
	} else if res.BranchDeleted {
		fmt.Printf("deleted branch %s (upstream contains it)\n", res.Branch)
	} else if res.BranchKept != "" {
		fmt.Printf("kept branch %s: %s\n", res.Branch, res.BranchKept)
	}
}

// failRetire reports a refusal in the requested shape and exits 1.
func failRetire(jsonOut bool, addr string, reason, detail any) {
	if jsonOut {
		writeJSON(map[string]any{"ok": false, "address": addr, "reason": reason, "detail": detail})
	} else {
		fmt.Fprintf(os.Stderr, "not retired %s: %s: %s\n", addr, reason, detail)
	}
	os.Exit(1)
}

// runGCCmd is `agentmux gc`: delete the leftovers of retired sessions
// older than the host retention (retention.yaml, default 14 days). With
// -dry-run it only lists what would go. The first real gc on each host
// should be a dry run shown to the operator for approval. Exit 0 done,
// 1 failed, 2 usage.
func runGCCmd(args []string) {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	dryRun := fs.Bool("dry-run", false, "list what would go without deleting anything")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "all", "host to collect (a name from hosts.yaml, \"local\", or \"all\")")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux gc [-json] [-dry-run] [-socket PATH] [-hosts PATH] [-host NAME]")
		os.Exit(2)
	}
	hosts, err := loadHosts(*hostsPath, *socketPath)
	if err != nil {
		log.Fatalf("gc: %v", err)
	}
	if *host != "all" {
		var filtered []hostsconfig.Host
		for _, h := range hosts {
			if h.Name == *host {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) == 0 {
			log.Fatalf("gc: host %q not found", *host)
		}
		hosts = filtered
	}
	type hostResult struct {
		host string
		res  ops.GCResult
		err  error
	}
	// gc runs per host: through the gateway when one is configured, else
	// locally when the entry is this machine. A tcp:// daemon address
	// without a gateway can't run gc — the operation needs the host's
	// own filesystem (retired records, amp CLI, sqlite), not a daemon
	// RPC that doesn't exist.
	results := make([]hostResult, 0, len(hosts))
	for _, h := range hosts {
		var res ops.GCResult
		var rerr error
		switch {
		case h.Gateway != "":
			ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.QueryTimeout+time.Minute)
			res, rerr = (&gatewayclient.Client{BaseURL: h.Gateway, Host: h.Name}).GC(ctx, gatewayapi.GCRequest{DryRun: *dryRun})
			cancel()
		case h.Address == "" || h.Address == "unix://"+*socketPath || address.Canonical(h.Name) == address.LocalHostName():
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			res, rerr = ops.Env{SocketPath: *socketPath}.GC(ctx, ops.GCRequest{DryRun: *dryRun})
			cancel()
		default:
			rerr = fmt.Errorf("host %q has no gateway in %s; gc needs the gateway to reach another host's filesystem", h.Name, *hostsPath)
		}
		results = append(results, hostResult{host: h.Name, res: res, err: rerr})
	}
	failed := false
	for _, r := range results {
		if r.err != nil {
			e := ops.AsError(r.err)
			fmt.Fprintf(os.Stderr, "gc on %s: %s: %s\n", r.host, e.Reason, e.Detail)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	if *jsonOut {
		out := make([]map[string]any, 0, len(results))
		for _, r := range results {
			out = append(out, map[string]any{"host": r.host, "gc": r.res.GCResult})
		}
		writeJSON(out)
		return
	}
	for _, r := range results {
		verb := "collected"
		if r.res.DryRun {
			verb = "would collect"
		}
		fmt.Printf("%s on %s (retention %dd):\n", verb, r.host, r.res.RetentionDays)
		for _, d := range r.res.Deleted {
			fmt.Printf("  - deleted %s (%s)\n", d.Instance, d.Agent)
		}
		for _, k := range r.res.Kept {
			fmt.Printf("  - keeping %s until %s\n", k.Instance, k.DeleteAt)
		}
		if len(r.res.Deleted) == 0 && len(r.res.Kept) == 0 {
			fmt.Println("  nothing retired")
		}
	}
}
