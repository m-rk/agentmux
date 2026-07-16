package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/m-rk/agentmux/daemon/internal/session"
)

// runSessionCmd is `agentmux session run|update|stop --instance NAME`: the
// per-instance unit's ExecStart/ExecStop, replacing rc-start.sh/
// rc-update.sh. Not meant to be run by hand. session.Run/Update/Stop
// dispatch to the right agent-specific implementation by reading the
// instance's own registry file.
func runSessionCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux session <run|update|stop> --instance NAME")
		os.Exit(1)
	}
	sub := args[0]
	fs := flag.NewFlagSet("session "+sub, flag.ExitOnError)
	instance := fs.String("instance", "", "instance name")
	fs.Parse(args[1:])
	if *instance == "" {
		fmt.Fprintln(os.Stderr, "-instance is required")
		os.Exit(1)
	}

	var err error
	switch sub {
	case "run":
		err = session.Run(*instance)
	case "update":
		err = session.Update(*instance)
	case "stop":
		err = session.Stop(*instance)
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand %q\n", sub)
		os.Exit(1)
	}
	if err != nil {
		log.Fatalf("session %s %s: %v", sub, *instance, err)
	}
}
