package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/m-rk/agentmux/daemon/internal/session"
)

// runUsageHook is the completed-turn usage hook (docs/completed-turn-usage.md):
// `usage-hook claude --instance NAME` is Claude Code's Stop hook, and
// `usage-hook codex --instance NAME --run ID --offset N` is run by the codex
// launcher with the run's log on stdin. It always exits 0: a hook must never
// fail or block the agent's turn, so problems go to the errors file.
func runUsageHook(args []string) {
	if len(args) == 0 || (args[0] != "claude" && args[0] != "codex") {
		fmt.Fprintln(os.Stderr, "usage: agentmux usage-hook claude|codex --instance NAME [--run ID --offset N]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("usage-hook "+args[0], flag.ExitOnError)
	instance := fs.String("instance", "", "agentmux instance name")
	run := fs.String("run", "", "codex launcher run id")
	offset := fs.String("offset", "0", "byte offset where the codex run's segment starts")
	_ = fs.Parse(args[1:])
	if *instance == "" {
		fmt.Fprintln(os.Stderr, "usage-hook: --instance is required")
		return
	}
	if args[0] == "claude" {
		session.UsageHookClaude(*instance, os.Stdin)
		return
	}
	n, err := session.ParseUsageOffset(*offset)
	if err != nil || *run == "" {
		fmt.Fprintln(os.Stderr, "usage-hook codex: --run and a numeric --offset are required")
		return
	}
	session.UsageHookCodex(*instance, *run, n, os.Stdin)
}
