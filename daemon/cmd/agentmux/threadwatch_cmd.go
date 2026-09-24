package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/threadwatch"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// runThreadwatchCmd is `agentmux threadwatch ...`: thread watch's own
// subcommands, dispatched the same way `agentmux notify ...` and
// `agentmux collab ...` dispatch theirs. See docs/thread-watch.md and
// docs/design/thread-watch.md.
func runThreadwatchCmd(args []string) {
	if len(args) == 0 {
		fmt.Println(`agentmux threadwatch: follow agent sessions and alert when they need you

Usage:
  agentmux threadwatch serve [-socket PATH] [-config PATH] [-interval 20s] [-dry-run] [-once]
                               run (or single-step) the poll loop
  agentmux threadwatch status [-since 24h] [-json]
                               show open intervene signals and recent Jev verdicts
  agentmux threadwatch install [-run-user USER] [-print]
                               (Linux, root) install agentmux-threadwatch.service
  agentmux threadwatch review ...
                               nightly digest (see its own -h)`)
		return
	}
	switch args[0] {
	case "serve":
		runThreadwatchServeCmd(args[1:])
	case "status":
		runThreadwatchStatusCmd(args[1:])
	case "install":
		runThreadwatchInstallCmd(args[1:])
	case "review":
		runThreadwatchReview(args[1:])
	case "-h", "--help", "help":
		runThreadwatchCmd(nil)
	default:
		log.Fatalf("threadwatch: unknown subcommand %q (want serve, status, install, or review)", args[0])
	}
}

// runThreadwatchServeCmd is `agentmux threadwatch serve`: builds a
// threadwatch.Runner wired to the local daemon (for ListInstances/ViewPane),
// the on-disk config/store/offsets, a Jev judge when TYPESAFE_API_KEY is
// set, and a Discord sender bound to the run user's webhook — then either
// runs it forever (Runner.Run) or steps it once (-once), per
// docs/design/thread-watch.md's "Phases".
func runThreadwatchServeCmd(args []string) {
	fs := flag.NewFlagSet("threadwatch serve", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on")
	configPath := fs.String("config", "", "threadwatch.yaml path (default ~/.config/agentmux/threadwatch.yaml)")
	interval := fs.Duration("interval", 0, "poll interval (default 20s)")
	dryRun := fs.Bool("dry-run", false, "print decisions instead of sending Discord alerts")
	once := fs.Bool("once", false, "run a single poll cycle and exit, instead of serving forever")
	fs.Parse(args)

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("threadwatch serve: resolving home directory: %v", err)
	}
	reexecUnderOp(home)

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = threadwatch.DefaultConfigPath(home)
	}
	cfg, err := threadwatch.LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("threadwatch serve: %v", err)
	}

	store, err := threadwatch.NewStore(threadwatch.StateDir(home))
	if err != nil {
		log.Fatalf("threadwatch serve: %v", err)
	}
	offsets, err := threadwatch.LoadOffsets(threadwatch.StateDir(home))
	if err != nil {
		log.Fatalf("threadwatch serve: %v", err)
	}

	client, err := tuiclient.Dial("local", "unix://"+*socketPath)
	if err != nil {
		log.Fatalf("threadwatch serve: %v", err)
	}
	defer client.Close()

	host, _ := os.Hostname()
	sender, note := threadwatchSender(home, *dryRun)
	if note != "" {
		log.Println(note)
	}

	judge := threadwatch.NewJudge(cfg)
	if judge == nil {
		log.Println("threadwatch: no TYPESAFE_API_KEY (or jev.mode: off); running deterministic-only")
	}

	runner := &threadwatch.Runner{
		Config:       cfg,
		Store:        store,
		Offsets:      offsets,
		Lister:       client,
		PaneViewer:   threadwatchPaneViewer{client},
		Judge:        judge,
		Alerter:      threadwatch.NewAlerter(cfg, host, sender, nil),
		Host:         host,
		PollInterval: *interval,
	}
	if *dryRun {
		runner.OnDecision = func(sig threadwatch.Signal, d threadwatch.Decision) {
			fmt.Printf("[%s] %s %s code=%s tier=%s page=%v reason=%s\n",
				sig.Time.Format(time.RFC3339), sig.Instance, shortThreadID(sig.Thread), sig.Code, sig.Tier, d.Page, d.Reason)
		}
	}

	ctx := context.Background()
	if *once {
		if err := runner.Cycle(ctx); err != nil {
			log.Fatalf("threadwatch serve: %v", err)
		}
		return
	}
	if err := runner.Run(ctx); err != nil {
		log.Fatalf("threadwatch serve: %v", err)
	}
}

// threadwatchSender builds the Sender Runner hands alerts to. -dry-run and
// "no webhook configured" both resolve to a no-op sender (Alerter still runs
// its dedup/rate-limit bookkeeping as if the page went out, which is what
// makes -dry-run's printed decisions meaningful) with a one-line note the
// caller logs once, instead of per alert.
func threadwatchSender(home string, dryRun bool) (threadwatch.Sender, string) {
	noop := func(string) error { return nil }
	if dryRun {
		return noop, "threadwatch: -dry-run: alerts will be printed, not sent"
	}
	path := discordnotify.PathForHome(home)
	cfg, err := discordnotify.Load(path)
	if err != nil {
		return noop, fmt.Sprintf("threadwatch: loading %s: %v; running without sending", path, err)
	}
	if cfg.WebhookURL == "" {
		return noop, fmt.Sprintf("threadwatch: no Discord webhook configured (%s); running without sending", path)
	}
	webhook := cfg.WebhookURL
	return func(message string) error {
		return discordnotify.Send(webhook, message)
	}, ""
}

// threadwatchPaneViewer adapts *tuiclient.Client's ViewPane RPC to
// threadwatch.PaneViewer, for the stall-detection fallback on instances
// whose agent has no structured collector.
type threadwatchPaneViewer struct {
	client *tuiclient.Client
}

func (v threadwatchPaneViewer) ViewPane(ctx context.Context, instance string) (string, error) {
	resp, err := v.client.ViewPane(ctx, &pb.ViewPaneRequest{Instance: instance})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// openSignal is one (instance, thread, code) key's latest signal, for
// `threadwatch status`.
type openSignal struct {
	Instance string                `json:"instance"`
	Thread   string                `json:"thread,omitempty"`
	Code     string                `json:"code"`
	Time     time.Time             `json:"time"`
	Reason   string                `json:"reason"`
	Judgment *threadwatch.Judgment `json:"judgment,omitempty"`
}

// runThreadwatchStatusCmd is `agentmux threadwatch status`: reads the local
// Store directly (no daemon round trip needed) and reports every intervene
// signal in the window that hasn't since been resolved, with counts and the
// most recent Jev verdict per instance.
func runThreadwatchStatusCmd(args []string) {
	fs := flag.NewFlagSet("threadwatch status", flag.ExitOnError)
	since := fs.Duration("since", 24*time.Hour, "how far back to look")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON instead of a table")
	fs.Parse(args)

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("threadwatch status: resolving home directory: %v", err)
	}
	store, err := threadwatch.NewStore(threadwatch.StateDir(home))
	if err != nil {
		log.Fatalf("threadwatch status: %v", err)
	}

	until := time.Now()
	from := until.Add(-*since)
	signals, err := store.ReadSignals(from, until)
	if err != nil {
		log.Fatalf("threadwatch status: %v", err)
	}

	open := openIntervene(signals)
	rows := make([]openSignal, 0, len(open))
	for _, s := range open {
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Instance != rows[j].Instance {
			return rows[i].Instance < rows[j].Instance
		}
		return rows[i].Time.Before(rows[j].Time)
	})

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			log.Fatalf("threadwatch status: %v", err)
		}
		return
	}

	if len(rows) == 0 {
		fmt.Printf("no open intervene signals in the last %s\n", *since)
		return
	}
	fmt.Printf("%-16s %-10s %-14s %-20s %s\n", "INSTANCE", "THREAD", "CODE", "SINCE", "JEV")
	for _, r := range rows {
		fmt.Printf("%-16s %-10s %-14s %-20s %s\n",
			r.Instance, shortThreadID(r.Thread), r.Code, r.Time.Local().Format("2006-01-02 15:04:05"), jevSummary(r.Judgment))
	}
}

// openIntervene reduces signals (in roughly chronological order, per
// Store.ReadSignals) to the latest, still-unresolved entry for every
// (instance, thread, code) key that is intervene tier.
func openIntervene(signals []threadwatch.Signal) map[string]openSignal {
	open := map[string]openSignal{}
	for _, sig := range signals {
		if sig.Tier != threadwatch.TierIntervene {
			continue
		}
		key := sig.Instance + "\x00" + sig.Thread + "\x00" + sig.Code
		if sig.Resolved {
			delete(open, key)
			continue
		}
		open[key] = openSignal{
			Instance: sig.Instance,
			Thread:   sig.Thread,
			Code:     sig.Code,
			Time:     sig.Time,
			Reason:   sig.Reason,
			Judgment: sig.Judgment,
		}
	}
	return open
}

func jevSummary(j *threadwatch.Judgment) string {
	if j == nil {
		return "-"
	}
	if j.Err != "" {
		return "error: " + j.Err
	}
	if j.WaitingKind != "" {
		return fmt.Sprintf("waiting=%s urgency=%.1f", j.WaitingKind, j.Urgency)
	}
	return fmt.Sprintf("urgency=%.1f conf=%.2f", j.Urgency, j.UrgencyConf)
}

func shortThreadID(thread string) string {
	if len(thread) <= 8 {
		return thread
	}
	return thread[:8]
}

// threadwatchOpMarker is set on serve's environment once it runs under
// `op run`, so a reference that resolves to nothing can't cause an exec loop.
const threadwatchOpMarker = "AGENTMUX_THREADWATCH_OP"

// reexecUnderOp restarts serve under `op run` when the user has an env-file
// at ~/.agentmux/env/threadwatch.env (normally holding TYPESAFE_API_KEY as an
// op:// reference) and the key isn't already in the environment. Doing this
// at startup rather than in the unit means adding or removing the env-file
// only needs a service restart, not a reinstall. Failure is logged and serve
// continues deterministic-only: a broken 1Password setup must not stop
// alerting.
func reexecUnderOp(home string) {
	envFile := filepath.Join(home, ".agentmux", "env", "threadwatch.env")
	if os.Getenv(threadwatchOpMarker) != "" || os.Getenv("TYPESAFE_API_KEY") != "" {
		return
	}
	if info, err := os.Stat(envFile); err != nil || !info.Mode().IsRegular() {
		return
	}
	self, err := os.Executable()
	if err != nil {
		log.Printf("threadwatch serve: not starting under op (resolving executable: %v); Jev disabled", err)
		return
	}
	argv := append([]string{self}, os.Args[1:]...)
	if err := session.ExecWithOpEnv(envFile, threadwatchOpMarker, argv); err != nil {
		log.Printf("threadwatch serve: not starting under op (%v); Jev disabled", err)
	}
}

// runThreadwatchInstallCmd is `agentmux threadwatch install -run-user USER`:
// writes and enables agentmux-threadwatch.service, running as USER (not
// root — see docs/design/thread-watch.md's "Running it and secrets") so it
// can read that user's own transcripts. The unit always runs `agentmux
// threadwatch serve` from the stable install path; serve itself moves under
// `op run` when the user has an env-file (see reexecUnderOp).
// threadwatchBin is where `agentmux daemon install` puts the binary.
const threadwatchBin = "/usr/local/bin/agentmux"

func runThreadwatchInstallCmd(args []string) {
	fs := flag.NewFlagSet("threadwatch install", flag.ExitOnError)
	runUser := fs.String("run-user", "", "OS user to run thread watch as (required)")
	print := fs.Bool("print", false, "print the systemd unit instead of installing it")
	fs.Parse(args)

	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "threadwatch install: not supported yet")
		os.Exit(1)
	}
	if *runUser == "" {
		log.Fatal("threadwatch install: -run-user is required")
	}
	_, err := user.Lookup(*runUser)
	if err != nil {
		log.Fatalf("threadwatch install: looking up user %q: %v", *runUser, err)
	}

	execStart := threadwatchBin + " threadwatch serve"
	if _, err := os.Stat(threadwatchBin); err != nil {
		log.Fatalf("threadwatch install: %s not found; run `sudo agentmux daemon install` first", threadwatchBin)
	}

	unit := fmt.Sprintf(threadwatchUnitTemplate, *runUser, execStart)

	if *print {
		fmt.Print(unit)
		return
	}

	if os.Geteuid() != 0 {
		log.Fatalf("threadwatch install: must be run as root; try: sudo agentmux threadwatch install -run-user %s", *runUser)
	}

	const unitPath = "/etc/systemd/system/agentmux-threadwatch.service"
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		log.Fatalf("threadwatch install: writing %s: %v", unitPath, err)
	}
	if err := runSystemctlThreadwatch("daemon-reload"); err != nil {
		log.Fatalf("threadwatch install: %v", err)
	}
	if err := runSystemctlThreadwatch("enable", "--now", "agentmux-threadwatch.service"); err != nil {
		log.Fatalf("threadwatch install: %v", err)
	}
	fmt.Printf("Installed and started agentmux-threadwatch.service as %s (unit: %s)\n", *runUser, unitPath)
}

const threadwatchUnitTemplate = `[Unit]
Description=agentmux thread watch
After=agentmuxd.service network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%[1]s
ExecStart=%[2]s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`

func runSystemctlThreadwatch(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", args[0], err)
	}
	return nil
}
