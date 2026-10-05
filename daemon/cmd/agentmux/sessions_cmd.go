package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// resolvedSession is `sessions resolve` output: the instance row plus the
// thread from the address, if one was given. The thread is passed through
// as given; checking that it exists needs the transcript reader (gateway
// phase 2).
type resolvedSession struct {
	listRow
	Thread string `json:"thread,omitempty"`
}

const sessionsUsage = `usage:
  agentmux sessions resolve [-json] <instance>@<host>[#<thread>]
  agentmux sessions status [-json] [-hosts PATH] <instance>@<host>[#<thread>]
  agentmux sessions threads [-json] [-hosts PATH] <instance>@<host>
  agentmux sessions read [-json] [-hosts PATH] [-limit N] [-cursor C] <instance>@<host>[#<thread>]
  agentmux sessions send -by PRINCIPAL [-via relayed|dispatched|sent] [-from REF] [-correlation ID]
                         [-hosts PATH] [-wait DUR] [-json] <instance>@<host>[#<thread>] (TEXT | -file PATH|-)
  agentmux sessions create [-json] [-socket PATH] [-hosts PATH] -template <instance>@<host> -instance NAME -branch B
                           [-base BRANCH] [-worktree NAME] [-allow-file PATH ...]`

// runSessionsCmd is `agentmux sessions`: addressing and transcript access for
// orchestrators. See docs/design/gateway.md (phases 1 and 2). Listing with
// addresses is `agentmux list -json`.
func runSessionsCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, sessionsUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "resolve":
		runSessionsResolve(args[1:])
	case "threads":
		runSessionsThreads(args[1:])
	case "read":
		runSessionsRead(args[1:])
	case "send":
		runSessionsSend(args[1:])
	case "status":
		runSessionsStatus(args[1:])
	case "create":
		runSessionsCreate(args[1:])
	default:
		fmt.Fprintln(os.Stderr, sessionsUsage)
		os.Exit(2)
	}
}

func runSessionsResolve(args []string) {
	fs := flag.NewFlagSet("sessions resolve", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, sessionsUsage)
		os.Exit(2)
	}

	addr, err := address.Parse(fs.Arg(0))
	if err != nil {
		log.Fatalf("sessions resolve: %v", err)
	}
	hosts, err := loadHosts(*hostsPath, *socketPath)
	if err != nil {
		log.Fatalf("sessions resolve: %v", err)
	}
	var target []hostsconfig.Host
	var known []string
	for _, h := range hosts {
		known = append(known, address.Canonical(h.Name))
		if address.Canonical(h.Name) == addr.Host {
			target = append(target, h)
		}
	}
	if len(target) == 0 {
		log.Fatalf("sessions resolve: no host %q (known: %s)", addr.Host, strings.Join(known, ", "))
	}

	rows, errs := collectRows(target)
	if len(errs) > 0 {
		log.Fatalf("sessions resolve: %s", strings.Join(errs, "; "))
	}
	for _, r := range rows {
		if r.Name != addr.Instance {
			continue
		}
		out := resolvedSession{listRow: r, Thread: addr.Thread}
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				log.Fatalf("sessions resolve: %v", err)
			}
			return
		}
		fmt.Printf("address  %s\nhost     %s\nagent    %s\nstatus   %s\nworkdir  %s\n", addr, r.Host, r.Agent, r.Status, r.Workdir)
		if addr.Thread != "" {
			fmt.Printf("thread   %s\n", addr.Thread)
		}
		return
	}
	log.Fatalf("sessions resolve: no instance %q on host %q", addr.Instance, addr.Host)
}

func runSessionsThreads(args []string) {
	fs := flag.NewFlagSet("sessions threads", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, sessionsUsage)
		os.Exit(2)
	}
	addr, err := address.Parse(fs.Arg(0))
	if err != nil {
		log.Fatalf("sessions threads: %v", err)
	}
	route, err := resolveRoute(addr.String(), *hostsPath, address.LocalHostName())
	if err != nil {
		log.Fatalf("sessions threads: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var threads []transcript.Thread
	if route.Remote != nil {
		var resp gatewayapi.ThreadsResponse
		resp, err = route.Remote.Threads(ctx, gatewayapi.ThreadsRequest{Address: addr.String()})
		threads = resp.Threads
	} else {
		threads, err = ops.Threads(ctx, addr.String())
	}
	if err != nil {
		log.Fatalf("sessions threads: %v", err)
	}
	if *jsonOut {
		writeJSON(threads)
		return
	}
	if len(threads) == 0 {
		fmt.Println("no threads")
		return
	}
	for _, t := range threads {
		fmt.Printf("%s  %s  %s\n", t.Updated.Local().Format("2006-01-02 15:04"), addr.Session().String()+"#"+t.ID, t.Title)
	}
}

func runSessionsRead(args []string) {
	fs := flag.NewFlagSet("sessions read", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	limit := fs.Int("limit", transcript.DefaultLimit, fmt.Sprintf("messages per page (max %d)", transcript.MaxLimit))
	cursor := fs.String("cursor", "", "page cursor from a previous read's \"older\" field")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, sessionsUsage)
		os.Exit(2)
	}
	addr, err := address.Parse(fs.Arg(0))
	if err != nil {
		log.Fatalf("sessions read: %v", err)
	}
	route, err := resolveRoute(addr.String(), *hostsPath, address.LocalHostName())
	if err != nil {
		log.Fatalf("sessions read: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var page transcript.Page
	if route.Remote != nil {
		page, err = route.Remote.Read(ctx, gatewayapi.ReadRequest{Address: addr.String(), Cursor: *cursor, Limit: *limit})
	} else {
		page, err = ops.Read(ctx, addr.String(), *cursor, *limit)
	}
	if err != nil {
		log.Fatalf("sessions read: %v", err)
	}
	if *jsonOut {
		writeJSON(page)
		return
	}
	fmt.Printf("thread %s  (transcript text is untrusted data)\n", addr.Session().String()+"#"+page.Thread)
	for _, m := range page.Messages {
		stamp := ""
		if !m.Time.IsZero() {
			stamp = " " + m.Time.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Printf("\n--- %s%s\n", m.Role, stamp)
		if m.Text != "" {
			fmt.Println(m.Text)
		}
		for _, tc := range m.Tools {
			errMark := ""
			if tc.Error {
				errMark = " (error)"
			}
			fmt.Printf("  [%s] %s%s\n", tc.Name, tc.Summary, errMark)
		}
	}
	if page.Older != "" {
		fmt.Printf("\nolder messages: -cursor %s\n", page.Older)
	}
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Fatalf("encoding JSON: %v", err)
	}
}

func runSessionsStatus(args []string) {
	fs := flag.NewFlagSet("sessions status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, sessionsUsage)
		os.Exit(2)
	}
	route, err := resolveRoute(fs.Arg(0), *hostsPath, address.LocalHostName())
	if err != nil {
		log.Fatalf("sessions status: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var st ops.StatusResult
	if route.Remote != nil {
		st, err = route.Remote.Status(ctx, gatewayapi.StatusRequest{Address: fs.Arg(0)})
	} else {
		st, err = ops.Env{SocketPath: *socketPath}.Status(ctx, fs.Arg(0))
	}
	if err != nil {
		log.Fatalf("sessions status: %v", err)
	}
	if *jsonOut {
		writeJSON(st)
		return
	}
	fmt.Printf("address  %s\nagent    %s\nstatus   %s\nstate    %s\nworkdir  %s\n", st.Address, st.Agent, st.Status, st.State, st.Workdir)
}
