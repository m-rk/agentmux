package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/modelsource"
)

const modelsUsage = "usage: agentmux models list [-json] [-config PATH]"

func runModelsCmd(args []string) {
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(os.Stderr, modelsUsage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("models list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	path := fs.String("config", modelsource.DefaultPath(), "model source registry path")
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
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"sources": results})
		return
	}
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
