package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/session"
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
  agentmux sessions threads [-json] <instance>@<host>
  agentmux sessions read [-json] [-limit N] [-cursor C] <instance>@<host>[#<thread>]`

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

// localTranscriptSource builds the transcript source for an address on this
// machine from the instance's registry entry. Reading another host's records
// needs the gateway (phase 4), so a remote address is refused here.
func localTranscriptSource(addr address.Address) (transcript.Source, transcript.Reader, error) {
	if addr.Host != address.LocalHostName() {
		return transcript.Source{}, nil, fmt.Errorf("%s is not this host (%s); reading another host's transcript needs the gateway", addr.Host, address.LocalHostName())
	}
	fields, err := session.ReadRegistry(addr.Instance)
	if err != nil {
		return transcript.Source{}, nil, fmt.Errorf("no instance %q on this host: %w", addr.Instance, err)
	}
	src := transcript.Source{
		Instance:    addr.Instance,
		Agent:       fields["AGENTMUX_AGENT"],
		Workdir:     fields["AGENTMUX_WORKDIR"],
		Home:        runas.CurrentUserHome(),
		AmpRunnerID: fields["AGENTMUX_AMP_RUNNER_ID"],
	}
	if src.Agent == "" {
		src.Agent = "claude-code" // claude-code registry entries predate AGENTMUX_AGENT
	}
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := user.Lookup(runUser); err == nil {
			src.Home = u.HomeDir
		}
	}
	if src.Agent == "amp" {
		envFile := filepath.Join(src.Home, ".agentmux", "env", addr.Instance+".env")
		if info, err := os.Stat(envFile); err == nil && info.Mode().IsRegular() {
			src.AmpEnvFile = envFile
		}
	}
	r, err := transcript.For(src.Agent)
	if err != nil {
		return transcript.Source{}, nil, err
	}
	return src, r, nil
}

func runSessionsThreads(args []string) {
	fs := flag.NewFlagSet("sessions threads", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
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
	src, r, err := localTranscriptSource(addr)
	if err != nil {
		log.Fatalf("sessions threads: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	threads, err := r.Threads(ctx, src)
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
	src, r, err := localTranscriptSource(addr)
	if err != nil {
		log.Fatalf("sessions read: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	page, err := r.Read(ctx, src, addr.Thread, *cursor, *limit)
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
