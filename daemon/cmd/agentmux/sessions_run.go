package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// runOutput is `sessions run -json` on success.
type runOutput struct {
	OK bool `json:"ok"`
	ops.RunResult
}

// runSessionsRun is `agentmux sessions run`: start an amp thread on an
// instance (or continue one with -thread) by running a prompt through the
// amp CLI in the instance's workdir, locally or through that host's
// gateway. -template is dry-run only: validate against an existing amp
// instance instead of the target, for targets that don't exist yet (the
// deploy smoke test's dry-run create makes nothing). See docs/amp-run.md
// and ops.Env.Run. Exit 0 ran, 1 refused or failed, 2 usage.
func runSessionsRun(args []string) {
	fs := flag.NewFlagSet("sessions run", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON (also on refusal)")
	file := fs.String("file", "", "read the prompt from this file (\"-\" for stdin) instead of the argument (required)")
	thread := fs.String("thread", "", "continue this amp thread id instead of starting a new thread")
	title := fs.String("title", "", "title the thread (\"<task id> <task name>\" from the dispatcher); re-applied after every run since amp's auto-title overwrites it")
	var labels labelFlags
	fs.Var(&labels, "label", "amp thread label; repeatable, rides -l on every run so `amp threads list --label X` finds it")
	mode := fs.String("mode", "", "explicit amp -m/--mode for this run only, overriding the host file and the instance override")
	dryRun := fs.Bool("dry-run", false, "validate the run without starting any amp thread (deploy smoke test)")
	template := fs.String("template", "", "dry-run only: validate against this existing amp instance instead of the target, which may not exist yet (deploy smoke test)")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	fs.Parse(args)
	if fs.NArg() != 1 || *file == "" {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions run [-json] [-dry-run] [-socket PATH] [-hosts PATH] [-thread THREAD_ID] [-title TEXT] [-label LABEL ...] [-mode MODE] [-template NAME] -file PATH|- <instance>@<host>[#<thread>]")
		os.Exit(2)
	}
	// Task sessions refuse unless this is itself a dry run (see liveguard),
	// so a task instance cannot start threads on another session.
	if !*dryRun {
		if err := liveguard.Check(); err != nil {
			failRun(*jsonOut, fs.Arg(0), safesend.ReasonForbidden, err.Error())
		}
	}
	addrText := fs.Arg(0)
	if *thread != "" {
		addr, err := address.Parse(addrText)
		if err != nil {
			failRun(*jsonOut, addrText, safesend.ReasonInvalid, err.Error())
		}
		if addr.Thread != "" && addr.Thread != *thread {
			failRun(*jsonOut, addrText, safesend.ReasonInvalid,
				fmt.Sprintf("address names thread %s but -thread says %s", addr.Thread, *thread))
		}
		addr.Thread = *thread
		addrText = addr.String()
	}

	text, err := readRunText(fs.Arg(0), *file)
	if err != nil {
		failRun(*jsonOut, addrText, safesend.ReasonInvalid, err.Error())
	}

	req := ops.RunRequest{Address: addrText, Text: text, Title: *title, Labels: labels, Mode: *mode, DryRun: *dryRun, Template: *template}
	var res ops.RunResult
	route, rerr := resolveRoute(req.Address, *hostsPath, address.LocalHostName())
	switch {
	case rerr != nil:
		e := ops.AsError(rerr)
		failRun(*jsonOut, req.Address, e.Reason, e.Detail)
	case route.Remote != nil:
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.RunTimeout+time.Minute)
		defer cancel()
		var rerr error
		res, rerr = route.Remote.Run(ctx, gatewayapi.RunRequest{Address: req.Address, Text: req.Text, Title: req.Title, Labels: req.Labels, Mode: req.Mode, DryRun: req.DryRun, Template: req.Template})
		if rerr != nil {
			e := ops.AsError(rerr)
			failRun(*jsonOut, req.Address, e.Reason, e.Detail)
		}
	default:
		ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.RunTimeout+time.Minute)
		defer cancel()
		var rerr error
		res, rerr = ops.Env{SocketPath: *socketPath}.Run(ctx, req)
		if rerr != nil {
			e := ops.AsError(rerr)
			failRun(*jsonOut, req.Address, e.Reason, e.Detail)
		}
	}

	if *jsonOut {
		writeJSON(runOutput{OK: true, RunResult: res})
		return
	}
	if res.DryRun {
		fmt.Printf("would run on %s\n", res.Address)
		for _, step := range res.Plan {
			fmt.Printf("  - %s\n", step)
		}
		return
	}
	fmt.Printf("thread   %s\nurl      %s\nstate    %s\n", res.Address, res.ThreadURL, res.State)
}

// labelFlags is a repeatable -label flag.
type labelFlags []string

func (l *labelFlags) String() string     { return "LABEL" }
func (l *labelFlags) Set(v string) error { *l = append(*l, v); return nil }

// failRun reports a refusal in the requested shape and exits 1.
func failRun(jsonOut bool, addr string, reason safesend.Reason, detail string) {
	if jsonOut {
		writeJSON(map[string]any{"ok": false, "address": addr, "reason": reason, "detail": detail})
	} else {
		fmt.Fprintf(os.Stderr, "not run on %s: %s: %s\n", addr, reason, detail)
	}
	os.Exit(1)
}

// readRunText reads the prompt from file ("-" for stdin). The address
// argument is unused except for symmetry with readSendText; the prompt
// never comes from the command line, so shell history can't leak it.
func readRunText(_ string, file string) (string, error) {
	if file == "-" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, safesend.MaxTextBytes+1))
		if err != nil {
			return "", err
		}
		if err := checkRunText(string(data)); err != nil {
			return "", err
		}
		return string(data), nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if err := checkRunText(string(data)); err != nil {
		return "", err
	}
	return string(data), nil
}

// checkRunText applies the send text limits without the provenance
// wrapper: a run prompt is a fresh thread start, not a paste into a TUI.
func checkRunText(raw string) error {
	if _, err := ops.CleanText(raw); err != nil {
		return err
	}
	return nil
}
