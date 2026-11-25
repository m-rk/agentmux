package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/probe"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/usagehook"
)

const modelsProbeUsage = "usage: agentmux models probe [-json] [-host H] [-catalogue PATH]"

// runModelsProbe is `agentmux models probe`: one tiny request to each
// provider tank this host's catalogue lists, saved for `models list`. It
// probes this host only; other hosts run their own and report through
// their gateway.
func runModelsProbe(args []string) {
	fs := flag.NewFlagSet("models probe", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print the rows as JSON")
	host := fs.String("host", "local", "host to probe: only this host (\"local\" or its own name)")
	catPath := fs.String("catalogue", catalogue.DefaultPath(), "model catalogue config path")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, modelsProbeUsage)
		os.Exit(2)
	}
	localName := address.LocalHostName()
	if *host != "local" && *host != localName {
		fmt.Fprintf(os.Stderr, "models probe: %s probes itself (its own timer) and reports through its gateway; run this on the host it checks\n", *host)
		os.Exit(2)
	}
	cfg, err := catalogue.Load(*catPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models probe: %v\n", err)
		os.Exit(1)
	}
	targets := probe.Targets(cfg, "local")
	if localName != "" {
		targets = mergeTargets(targets, probe.Targets(cfg, localName))
	}
	statePath := catalogue.HealthPath(runas.CurrentUserHome())
	st, err := catalogue.LoadHealth(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models probe: reading state: %v\n", err)
		os.Exit(1)
	}
	eventsPath, evErr := usagehook.EventsPath()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := probe.Options{
		Try: probe.Exec(session.ProbeSetup, &probe.AmpStanding{
			Path: probe.AmpThreadPath(runas.CurrentUserHome()),
			Now:  time.Now,
			Log:  func(line string) { fmt.Fprintln(os.Stderr, "models probe: "+line) },
		}),
		Sleep:   sleepCtx,
		Now:     time.Now,
		Backoff: probe.DefaultBackoff,
		Logged: func(t probe.Target, try int, a probe.Attempt) {
			ev, ok := probe.UsageEvent(t, "local", try, a, time.Now())
			if !ok || evErr != nil {
				return
			}
			if err := usagehook.Append(eventsPath, ev); err != nil {
				fmt.Fprintf(os.Stderr, "models probe: usage line for %s: %v\n", t.Provider, err)
			}
		},
	}
	rows := probe.Run(ctx, opts, targets, "local", localName, st.Providers)
	st.Providers = probe.Merge(st.Providers, "local", rows)
	if err := catalogue.SaveHealth(statePath, st); err != nil {
		fmt.Fprintf(os.Stderr, "models probe: saving state: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"providers": rows})
		return
	}
	if len(rows) == 0 {
		fmt.Println("No providers configured for this host in the catalogue.")
		return
	}
	printProviders(rows)
}

func mergeTargets(a, b []probe.Target) []probe.Target {
	seen := map[string]bool{}
	for _, t := range a {
		seen[t.Provider] = true
	}
	for _, t := range b {
		if !seen[t.Provider] {
			seen[t.Provider] = true
			a = append(a, t)
		}
	}
	return a
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// printProviders is the plain-text view of probe rows.
func printProviders(rows []catalogue.ProviderHealth) {
	for _, r := range rows {
		status := r.Status
		if r.Status == catalogue.HealthOK {
			status = fmt.Sprintf("ok (%dms)", r.LatencyMS)
		} else if r.Error != "" {
			status += ": " + r.Error
		}
		lastOK := r.LastOKAt
		if lastOK == "" {
			lastOK = "-"
		}
		fmt.Printf("provider\t%s\t%s\t%s\t%s\tlast ok %s\n", r.Provider, r.Model, r.HostLabel, status, lastOK)
	}
}
