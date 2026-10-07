package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

// createOutput is `sessions create -json` on success; a refusal prints
// {"ok": false, "reason", "detail"}.
type createOutput struct {
	OK bool `json:"ok"`
	ops.CreateResult
}

// runSessionsCreate is `agentmux sessions create`: start a task session in a
// new Git worktree on the template's host, locally or through that host's
// gateway. See docs/design/gateway.md (phase 4b) and ops.Env.Create. Exit 0
// created or reused, 1 refused or failed, 2 usage.
func runSessionsCreate(args []string) {
	fs := flag.NewFlagSet("sessions create", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON (also on refusal)")
	template := fs.String("template", "", "address of an existing instance on the target host to copy agent, model and provider from (required)")
	instance := fs.String("instance", "", "name of the new instance (required)")
	branch := fs.String("branch", "", "branch the worktree is on; created if it doesn't exist (required)")
	base := fs.String("base", "", "origin branch a new worktree branch starts from: fetched first, refused if the fetch fails or origin/BASE is missing (default: origin/HEAD's target, else HEAD)")
	worktree := fs.String("worktree", "", "worktree directory name (default: the instance name)")
	var allowFiles stringList
	fs.Var(&allowFiles, "allow-file", "absolute path, on the target host, of one file outside the worktree the agent may read and edit (repeatable)")
	dryRun := fs.Bool("dry-run", false, "check template, names and origin base without creating anything (deploy smoke test)")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	fs.Parse(args)
	if fs.NArg() != 0 || *template == "" || *instance == "" || *branch == "" {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions create [-json] [-dry-run] [-socket PATH] [-hosts PATH] -template <instance>@<host> -instance NAME -branch B [-base BRANCH] [-worktree NAME] [-allow-file PATH ...]")
		os.Exit(2)
	}

	req := ops.CreateRequest{
		Template: *template, Instance: *instance, Branch: *branch,
		Base: *base, Worktree: *worktree, AllowFiles: allowFiles,
		DryRun: *dryRun,
	}
	var res ops.CreateResult
	route, err := resolveRoute(req.Template, *hostsPath, address.LocalHostName())
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.CreateTimeout+time.Minute)
		defer cancel()
		if route.Remote != nil {
			res, err = route.Remote.Create(ctx, gatewayapi.CreateRequest{
				Template: req.Template, Instance: req.Instance, Branch: req.Branch,
				Base: req.Base, Worktree: req.Worktree, AllowFiles: req.AllowFiles,
				DryRun: req.DryRun,
			})
		} else {
			res, err = ops.Env{SocketPath: *socketPath}.Create(ctx, req)
		}
	}

	if err != nil {
		e := ops.AsError(err)
		if *jsonOut {
			writeJSON(map[string]any{"ok": false, "reason": e.Reason, "detail": e.Detail})
		} else {
			fmt.Fprintf(os.Stderr, "not created: %s: %s\n", e.Reason, e.Detail)
		}
		os.Exit(1)
	}
	if *jsonOut {
		writeJSON(createOutput{OK: true, CreateResult: res})
		return
	}
	verb := "created"
	if !res.Created {
		verb = "reused existing"
	}
	if res.DryRun {
		fmt.Printf("would create %s\nworkdir  %s\nbranch   %s\n", res.Address, res.Workdir, res.Branch)
		for _, step := range res.Plan {
			fmt.Printf("  - %s\n", step)
		}
	} else {
		fmt.Printf("%s %s\nworkdir  %s\nbranch   %s\n", verb, res.Address, res.Workdir, res.Branch)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if res.BaseCommit != "" {
		fmt.Printf("base     origin/%s @ %s\n", res.Base, res.BaseCommit)
	}
}
