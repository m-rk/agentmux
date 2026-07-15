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
// rc-update.sh. Not meant to be run by hand; only claude-code is wired up
// so far (Phase B) — zero/opencode land in a follow-up.
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
		err = session.RunClaudeCode(*instance)
	case "update":
		err = session.UpdateClaudeCode(*instance)
	case "stop":
		err = session.StopClaudeCode(*instance)
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand %q\n", sub)
		os.Exit(1)
	}
	if err != nil {
		log.Fatalf("session %s %s: %v", sub, *instance, err)
	}
}
