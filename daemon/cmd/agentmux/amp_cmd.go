package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"

	"github.com/m-rk/agentmux/daemon/internal/ampsweep"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// runAmpCmd is `agentmux amp`: amp-account housekeeping on this host.
// Today that is only `sweep` (see runAmpSweep); the group exists so
// later amp-account commands have a home that is not the top level.
func runAmpCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, ampUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "sweep":
		runAmpSweep(args[1:])
	default:
		fmt.Fprintln(os.Stderr, ampUsage)
		os.Exit(2)
	}
}

const ampUsage = `usage:
  agentmux amp sweep [-json] [-dry-run] [-run-user USER]`

// runAmpSweep is `agentmux amp sweep`: archive this host's amp account
// junk threads that belong to no task instance (see the ampsweep
// package). With -dry-run it only lists what would go. Exit 0 done, 1
// failed, 2 usage.
func runAmpSweep(args []string) {
	fs := flag.NewFlagSet("amp sweep", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	dryRun := fs.Bool("dry-run", false, "list what would go without archiving anything")
	runUser := fs.String("run-user", "", "OS user whose amp account to sweep (default: auto-detected like doctor)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, ampUsage)
		os.Exit(2)
	}

	sw := runLocalSweep(*dryRun, *runUser)
	if sw.err != nil {
		log.Fatalf("amp sweep: %v", sw.err)
	}
	res := sw.res

	if *jsonOut {
		writeJSON(map[string]any{"ok": true, "sweep": res})
		return
	}
	verb := "archived"
	if *dryRun {
		verb = "would archive"
	}
	fmt.Printf("examined %d threads, %s %d:\n", res.Examined, verb, len(res.Candidates))
	for _, c := range res.Candidates {
		title := c.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("  - %s %s %s: %s\n", c.ID, title, verb, c.Reason)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if len(res.Candidates) == 0 && len(res.Warnings) == 0 {
		fmt.Println("  nothing to archive")
	}
}

// discoveryInstances adapts the instance registry to ampsweep.Instances:
// every live (non-dead) amp instance with a runner id. Dead instances
// are skipped — a thread recorded for a dead instance is abandoned, not
// live, and the dispatched-prefix guard still keeps dispatched workers
// either way. The runner id comes from the registry file (discovery
// drops it), read through session.ReadRegistry.
type discoveryInstances struct{}

func (discoveryInstances) Live(ctx context.Context) ([]ampsweep.LiveInstance, error) {
	_ = ctx
	instances, err := discovery.List()
	if err != nil {
		return nil, err
	}
	var out []ampsweep.LiveInstance
	for _, in := range instances {
		if in.Agent != "amp" || in.Status == discovery.StatusDead {
			continue
		}
		fields, err := session.ReadRegistry(in.Name)
		if err != nil {
			continue
		}
		runner := fields["AGENTMUX_AMP_RUNNER_ID"]
		if runner == "" {
			continue
		}
		home := runas.CurrentUserHome()
		if in.RunUser != "" {
			if u, err := user.Lookup(in.RunUser); err == nil {
				home = u.HomeDir
			}
		}
		src := ampsweep.LiveInstance{Name: in.Name, Runner: runner, Home: home, RunUser: in.RunUser, Workdir: in.Workdir}
		out = append(out, src)
	}
	return out, nil
}
