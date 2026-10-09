package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

func runTemplateAdd(args []string) {
	fs := flag.NewFlagSet("sessions template add", flag.ExitOnError)
	from := fs.String("from", "", "source instance@host (required)")
	agent := fs.String("agent", "codex", "runner agent")
	name := fs.String("name", "", "new instance name")
	dry := fs.Bool("dry-run", false, "print plan without creating")
	jout := fs.Bool("json", false, "JSON output")
	sock := fs.String("socket", daemoninstall.SocketPath(), "local daemon socket")
	hp := fs.String("hosts", hostsconfig.DefaultPath(), "hosts config")
	fs.Parse(args)
	if *from == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions template add -from <instance>@<host> -agent codex [-name NAME] [-dry-run] [-json]")
		os.Exit(2)
	}
	route, err := resolveRoute(*from, *hp, address.LocalHostName())
	var res ops.RunnerAddResult
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.CreateTimeout)
		defer cancel()
		req := ops.RunnerAddRequest{From: *from, Agent: *agent, Name: *name, DryRun: *dry}
		if route.Remote != nil {
			var rr gatewayapi.TemplateAddResponse
			rr, err = route.Remote.AddRunner(ctx, gatewayapi.TemplateAddRequest{From: *from, Agent: *agent, Name: *name, DryRun: *dry})
			res = rr
		} else {
			res, err = (ops.Env{SocketPath: *sock}).AddRunner(ctx, req)
		}
	}
	if err != nil {
		e := ops.AsError(err)
		if *jout {
			writeJSON(map[string]any{"ok": false, "reason": e.Reason, "detail": e.Detail})
		} else {
			fmt.Fprintf(os.Stderr, "not added: %s: %s\n", e.Reason, e.Detail)
		}
		os.Exit(1)
	}
	if *jout {
		writeJSON(map[string]any{"ok": true, "result": res})
		return
	}
	if res.DryRun {
		fmt.Printf("would add %s (%s) to %s\n", res.Name, res.Agent, res.Project)
		for _, p := range res.Plan {
			fmt.Println("  - " + p)
		}
	} else {
		fmt.Printf("%s %s (%s) for %s\n", map[bool]string{true: "created", false: "reused"}[res.Created], res.Name, res.Agent, res.Project)
	}
}
