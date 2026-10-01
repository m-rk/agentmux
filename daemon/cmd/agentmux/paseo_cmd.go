package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/paseoupdate"
	"github.com/m-rk/agentmux/daemon/internal/runas"
)

const (
	paseoUnit             = "paseo-daemon.service"
	defaultPaseoUpdateAt  = "04:00"
	defaultPaseoUpdateBin = "/usr/local/bin/agentmux"
)

// runPaseoCmd is `agentmux paseo`. Today it only has `update`.
func runPaseoCmd(args []string) {
	if len(args) == 0 || args[0] != "update" {
		fmt.Fprintln(os.Stderr, "usage: agentmux paseo update [-check] [-run-user USER]\n       agentmux paseo update install [-at HH:MM] [-run-user USER] [-print]")
		os.Exit(2)
	}
	runPaseoUpdate(args[1:])
}

func runPaseoUpdate(args []string) {
	if len(args) > 0 && args[0] == "install" {
		runPaseoUpdateInstall(args[1:])
		return
	}

	fs := flag.NewFlagSet("paseo update", flag.ExitOnError)
	runUser := fs.String("run-user", "", "OS user that owns the paseo install and Discord webhook (default: auto-detected like doctor)")
	check := fs.Bool("check", false, "only report whether a newer stable release exists; change nothing")
	noNotify := fs.Bool("no-notify", false, "skip the Discord message")
	timeout := fs.Duration("timeout", 8*time.Minute, "overall timeout, including the health wait and any rollback")
	fs.Parse(args)

	identity, err := doctorIdentity(*runUser)
	if err != nil {
		log.Fatalf("paseo update: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	run := func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := runas.CommandContext(ctx, identity.Username, name, args...).CombinedOutput()
		return string(out), err
	}

	deps := paseoupdate.Deps{
		Run:     run,
		Restart: func(ctx context.Context) error { return restartPaseo(ctx, run, identity.HomeDir) },
		Logf:    func(f string, a ...any) { log.Printf("paseo update: "+f, a...) },
	}
	if !*noNotify && !*check {
		deps.Notify = paseoDiscordNotifier(identity.HomeDir)
	}

	// A supervisor still on the removed --foreground flag, or crash looping,
	// would fail the post-update health check and the rollback after it.
	if problem := paseoSupervisorProblem(ctx, identity.HomeDir); problem != "" {
		if !*check {
			log.Fatalf("paseo update: not updating: %s", problem)
		}
		fmt.Println("warning: " + problem)
	}

	// The desktop app runs a daemon bundled inside Paseo.app and updates it
	// with the app; the npm CLI is not what it runs.
	if out, err := run(ctx, "paseo", "daemon", "status", "--json"); err == nil {
		var st struct {
			DesktopManaged bool `json:"desktopManaged"`
		}
		if json.Unmarshal([]byte(out), &st) == nil && st.DesktopManaged {
			fmt.Println("paseo daemon is managed by the Paseo desktop app, which updates it. Nothing to do.")
			if *check {
				if res, err := paseoupdate.Update(ctx, deps, true); err == nil {
					fmt.Printf("paseo CLI %s installed, latest %s (the app's daemon does not use it)\n", res.From, res.To)
				}
			}
			return
		}
	}

	res, err := paseoupdate.Update(ctx, deps, *check)
	if err != nil {
		log.Fatalf("paseo update: %v", err)
	}
	switch res.Outcome {
	case paseoupdate.UpToDate:
		fmt.Printf("paseo %s is up to date\n", res.From)
	case paseoupdate.Available:
		fmt.Printf("paseo %s -> %s available (run without -check to update)\n", res.From, res.To)
	case paseoupdate.Updated:
		fmt.Printf("paseo updated %s -> %s\n", res.From, res.To)
	case paseoupdate.RolledBack:
		fmt.Printf("paseo update to %s failed (%s); rolled back to %s\n", res.To, res.Detail, res.From)
		os.Exit(1)
	case paseoupdate.RollbackFailed:
		fmt.Printf("paseo update to %s failed and rollback failed: %s\n", res.To, res.Detail)
		os.Exit(1)
	}
}

// restartPaseo restarts the daemon the way this host supervises it: through
// its systemd unit or LaunchAgent when one exists, otherwise with the paseo
// CLI's own stop/start for a daemon it daemonized itself.
func restartPaseo(ctx context.Context, run func(context.Context, string, ...string) (string, error), home string) error {
	sup, err := findPaseoSupervisor(ctx, home)
	if err != nil {
		return err
	}
	if sup != nil {
		return restartPaseoSupervisor(ctx, sup)
	}
	_, _ = run(ctx, "paseo", "daemon", "stop") // may already be stopped
	if out, err := run(ctx, "paseo", "daemon", "start"); err != nil {
		return fmt.Errorf("paseo daemon start: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// paseoDiscordNotifier sends to the run user's Discord webhook, or just logs
// when none is configured; a missing webhook must never fail the update.
func paseoDiscordNotifier(home string) func(string) {
	return func(message string) {
		cfg, err := discordnotify.Load(discordnotify.PathForHome(home))
		if err != nil || cfg == nil || cfg.WebhookURL == "" {
			log.Printf("paseo update: Discord not configured for this user; skipping notification")
			return
		}
		if err := discordnotify.Send(cfg.WebhookURL, message); err != nil {
			log.Printf("paseo update: sending Discord message: %v", err)
		}
	}
}

func runPaseoUpdateInstall(args []string) {
	fs := flag.NewFlagSet("paseo update install", flag.ExitOnError)
	at := fs.String("at", defaultPaseoUpdateAt, "local time (HH:MM) to check for a new release daily; pick a quiet hour, the daemon restarts on update")
	runUser := fs.String("run-user", "", "OS user that owns the paseo install (default: auto-detected like doctor)")
	bin := fs.String("bin", defaultPaseoUpdateBin, "agentmux binary path the scheduled job execs (macOS: ~/.agentmux/bin/agentmux)")
	print := fs.Bool("print", false, "print the unit files / plist without installing them")
	fs.Parse(args)

	identity, err := doctorIdentity(*runUser)
	if err != nil {
		log.Fatalf("paseo update install: %v", err)
	}
	if _, _, err := daemoninstall.ParseDoctorTime(*at); err != nil {
		log.Fatalf("paseo update install: invalid -at time %q: %v", *at, err)
	}
	binPath := *bin
	if runtime.GOOS == "darwin" && binPath == defaultPaseoUpdateBin {
		binPath = filepath.Join(identity.HomeDir, ".agentmux", "bin", "agentmux")
	}
	if err := installPaseoUpdateSchedule(identity.Username, identity.HomeDir, binPath, *at, *print); err != nil {
		log.Fatalf("paseo update install: %v", err)
	}
}
