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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/threadwatch"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
	"github.com/m-rk/agentmux/daemon/internal/typesafe"
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
                               nightly digest (see its own -h)
  agentmux threadwatch jev-test [-config PATH]
                               check the TypeSafe key with one synthetic judgment`)
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
	case "jev-test":
		runThreadwatchJevTestCmd(args[1:])
	case "-h", "--help", "help":
		runThreadwatchCmd(nil)
	default:
		log.Fatalf("threadwatch: unknown subcommand %q (want serve, status, install, review, or jev-test)", args[0])
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

	apiKey, keyNote := threadwatchAPIKey(context.Background(), cfg)
	judge := threadwatch.NewJudge(cfg, apiKey)
	if judge == nil {
		log.Printf("threadwatch: Jev off (%s; jev.mode %s); running deterministic-only", keyNote, cfg.Jev.Mode)
	} else {
		log.Printf("threadwatch: Jev %s (%s)", cfg.Jev.Mode, keyNote)
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

// threadwatchAPIKey finds the optional TypeSafe key: TYPESAFE_API_KEY from
// the environment, else threadwatch.yaml's jev.api_key, else its
// jev.api_key_ref resolved through 1Password. It returns where the key came
// from, or why there isn't one, for a log line that never includes the key.
func threadwatchAPIKey(ctx context.Context, cfg threadwatch.Config) (key, note string) {
	if v := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); v != "" {
		return v, "TypeSafe key from TYPESAFE_API_KEY"
	}
	jc := cfg.Jev
	switch {
	case jc.KeyProblem != "":
		return "", jc.KeyProblem
	case jc.APIKey != "":
		return jc.APIKey, "TypeSafe key from threadwatch.yaml"
	case jc.APIKeyRef != "":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		v, err := session.ReadOpRef(ctx, jc.APIKeyRef)
		if err != nil {
			return "", err.Error()
		}
		return v, "TypeSafe key from " + jc.APIKeyRef
	}
	return "", "no TypeSafe key configured"
}

// runThreadwatchInstallCmd is `agentmux threadwatch install -run-user USER`:
// writes and enables agentmux-threadwatch.service, running as USER (not
// root — see docs/design/thread-watch.md's "Running it and secrets") so it
// can read that user's own transcripts. The unit always runs `agentmux
// threadwatch serve` from the stable install path; the optional TypeSafe key
// comes from threadwatch.yaml (see threadwatchAPIKey).
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

// runThreadwatchJevTestCmd is `agentmux threadwatch jev-test`: resolves the
// configured TypeSafe key the same way serve does and asks Jev about one
// synthetic awaiting-user signal, printing the typed verdict (never the key).
func runThreadwatchJevTestCmd(args []string) {
	fs := flag.NewFlagSet("threadwatch jev-test", flag.ExitOnError)
	configPath := fs.String("config", "", "threadwatch.yaml path (default ~/.config/agentmux/threadwatch.yaml)")
	fs.Parse(args)

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("threadwatch jev-test: %v", err)
	}
	if *configPath == "" {
		*configPath = threadwatch.DefaultConfigPath(home)
	}
	cfg, err := threadwatch.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("threadwatch jev-test: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	key, note := threadwatchAPIKey(ctx, cfg)
	fmt.Println(note)
	if cfg.Jev.Mode == "off" {
		fmt.Println("jev.mode is off: serve will not call Jev")
	}
	if key == "" {
		os.Exit(1)
	}

	now := time.Now()
	sig := threadwatch.Signal{
		Time: now, Instance: "jev-test", Thread: "synthetic", Code: threadwatch.CodeAwaitingUser,
		Tier: threadwatch.TierIntervene, Reason: "idle for 12 minutes after the agent's last message",
	}
	judge := threadwatch.JevJudge{Client: &typesafe.Client{APIKey: key}, Model: cfg.Jev.Model}
	cases := []struct{ label, reply, expect string }{
		{"question", "I've migrated the settings page and the tests pass. The old form also backs the admin page. Should I migrate that too, or leave it for a separate change?",
			"question_to_user, needs_human_now high"},
		{"finished", "Done: the settings page now uses the new form component, all 214 tests pass, and I've pushed the branch. Nothing else is needed.",
			"finished, needs_human_now low"},
	}
	failed := false
	for _, c := range cases {
		recent := []threadwatch.Event{
			{Time: now.Add(-13 * time.Minute), Instance: "jev-test", Agent: "claude-code", Kind: threadwatch.KindUserMessage, Excerpt: "Please migrate the settings page to the new form component."},
			{Time: now.Add(-12 * time.Minute), Instance: "jev-test", Agent: "claude-code", Kind: threadwatch.KindAssistantMsg, Excerpt: c.reply},
		}
		j := judge.Judge(ctx, sig, recent)
		if j.Err != "" {
			fmt.Printf("%-9s Jev call failed: %s\n", c.label, j.Err)
			failed = true
			continue
		}
		fmt.Printf("%-9s waiting_kind=%s needs_human_now=%.2f urgency=%.1f (confidence %.2f) — expected %s\n",
			c.label, j.WaitingKind, j.NeedsHumanNow, j.Urgency, j.UrgencyConf, c.expect)
	}
	if failed {
		os.Exit(1)
	}
}
