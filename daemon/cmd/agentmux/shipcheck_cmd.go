package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/ops"
)

// runShipCheck is `agentmux ship-check`: the pre-ship hygiene gate for a
// task branch (AMUX-64). It refuses the ship when the branch's commits
// still carry agent/AI trailers, naming each offending commit, and it
// reports (advisory only) private hostnames and personal paths in the
// worktree's uncommitted changes. A go1com origin skips the trailer
// refusal; the privacy report applies everywhere. Exit 0 clean, 1 holds
// the ship, 2 usage or hard failure.
func runShipCheck(args []string) {
	fs := flag.NewFlagSet("ship-check", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	base := fs.String("base", "", "origin branch the task branch started from (default: the origin default branch)")
	workdir := fs.String("workdir", "", "worktree to scan for uncommitted privacy leaks (default: the current directory)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux ship-check [-json] [-base BRANCH] [-workdir PATH] <branch>")
		os.Exit(2)
	}
	branch := fs.Arg(0)

	repo, err := ops.GitTopLevelFor(branch, *workdir)
	if err != nil {
		if *jsonOut {
			writeShipCheckJSON(false, "failed", err.Error(), nil, nil)
		} else {
			fmt.Fprintf(os.Stderr, "ship-check: %v\n", err)
		}
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := ops.Env{}
	bad, err := env.CheckBranchTrailers(ctx, repo, branch, *base)
	if err != nil {
		e := ops.AsError(err)
		if *jsonOut {
			writeShipCheckJSON(false, string(e.Reason), e.Detail, nil, nil)
		} else {
			fmt.Fprintf(os.Stderr, "ship-check: %s: %s\n", e.Reason, e.Detail)
		}
		os.Exit(1)
	}
	wt := *workdir
	if wt == "" {
		wt, _ = os.Getwd()
	}
	privacy := env.ScanWorktreePrivacy(ctx, wt, 20)
	if *jsonOut {
		writeShipCheckJSON(len(bad) == 0, "", "", bad, privacy)
		if len(bad) > 0 {
			os.Exit(1)
		}
		return
	}
	for _, p := range privacy {
		fmt.Printf("ship-check privacy: %s\n", p)
	}
	if len(bad) > 0 {
		var names []string
		for _, f := range bad {
			names = append(names, f.Hash+" "+f.Subject)
		}
		fmt.Fprintf(os.Stderr, "ship-check: branch %s carries agent trailers (%s); remove them before shipping:\n  %s\n",
			branch, ops.ForbiddenTrailerNames, strings.Join(names, "\n  "))
		os.Exit(1)
	}
	fmt.Printf("ship-check: branch %s is clean\n", branch)
}

func writeShipCheckJSON(ok bool, reason, detail string, trailers []ops.TrailerFinding, privacy []string) {
	out := map[string]any{"ok": ok}
	if reason != "" {
		out["reason"] = reason
	}
	if detail != "" {
		out["detail"] = detail
	}
	if trailers == nil {
		out["trailers"] = []ops.TrailerFinding{}
	} else {
		out["trailers"] = trailers
	}
	if privacy == nil {
		out["privacy"] = []string{}
	} else {
		out["privacy"] = privacy
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
