package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/modelsource"
)

const modelsUsage = "usage: agentmux models list [-json] [-config PATH] [-catalogue PATH] [-hosts PATH]"

func runModelsCmd(args []string) {
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(os.Stderr, modelsUsage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("models list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	path := fs.String("config", modelsource.DefaultPath(), "model source registry path")
	catPath := fs.String("catalogue", catalogue.DefaultPath(), "model catalogue config path")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	_ = fs.Parse(args[1:])
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, modelsUsage)
		os.Exit(2)
	}
	r, err := modelsource.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models: %v\n", err)
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "models: %v\n", err)
		os.Exit(1)
	}
	results := modelsource.Check(context.Background(), r.Sources, exe, time.Now())
	cfg, err := catalogue.Load(*catPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	reports := checkHosts(ctx, cfg.Hosts(), time.Now(), localRunnerProbe, gatewayRunnerProbe(*hostsPath))
	cat := catalogue.Build(cfg, reports, address.LocalHostName())
	readFuel(ctx, &cat, catalogue.LocalRunners()[catalogue.RunnerAmp], catalogue.AmpChatGPTSubscription)
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"sources": results, "catalogue": cat})
		return
	}
	defer printCatalogue(cat)
	if len(results) == 0 {
		fmt.Println("No model sources configured.")
		return
	}
	for _, v := range results {
		status := "reachable"
		if !v.Reachable {
			status = "unavailable: " + v.Reason
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", v.Source.Name, v.Source.Kind, v.Source.Cost, status)
	}
}

// printCatalogue is the plain-text view: one line per offering and host.
func printCatalogue(cat catalogue.Catalogue) {
	for _, o := range cat.Offerings {
		name := o.Model
		if o.Mode != "" {
			name = "mode " + o.Mode
		}
		for _, a := range o.Availability {
			status := "available"
			if !a.Available {
				status = "unavailable: " + a.Reason
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Runner, name, o.Tier, o.Fuel, a.Host, status)
		}
	}
}
