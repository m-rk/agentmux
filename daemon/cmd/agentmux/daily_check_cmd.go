package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/dailycheck"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// runDoctorCmd is `agentmux doctor`: the manual form of the one-shot job
// installed alongside agentmuxd. It is intentionally local-host only; one
// scheduled doctor runs on each host shortly after that host's refresh window.
func runDoctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on")
	runUser := fs.String("run-user", "", "OS user whose Claude login and Discord webhook to use (Linux root jobs default to the first Claude Code instance owner)")
	checker := fs.String("checker", "claude", "analysis CLI used only when deterministic probes find trouble (Claude Code by default)")
	model := fs.String("model", "", "optional Claude model override (empty uses the user's Claude default)")
	paneLines := fs.Int("lines", 20, "trailing pane lines to include in each capped snapshot")
	maxPaneBytes := fs.Int("max-pane-bytes", 12*1024, "maximum pane bytes sent per session")
	dryRun := fs.Bool("dry-run", false, "report proposed repairs without applying them")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall check timeout")
	fs.Parse(args)

	identity, err := doctorIdentity(*runUser)
	if err != nil {
		log.Fatalf("doctor: %v", err)
	}
	if *paneLines < 0 || *maxPaneBytes < 1024 {
		log.Fatal("doctor: -lines must be non-negative and -max-pane-bytes must be at least 1024")
	}
	statePath := defaultDoctorStatePath(identity)
	previousState, stateErr := dailycheck.LoadNotificationState(statePath)

	client, err := tuiclient.Dial("local", "unix://"+*socketPath)
	if err != nil {
		log.Fatalf("doctor: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	command := dailycheck.CommandFactory(runas.CurrentUserCommandContext)
	if os.Geteuid() == 0 {
		command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return runas.CommandContext(ctx, identity.Username, name, args...)
		}
	}
	analyzer := dailycheck.ClaudeAnalyzer{
		Command: command,
		Binary:  *checker,
		Model:   *model,
		Dir:     identity.HomeDir,
	}
	report, runErr := dailycheck.Run(ctx, client, analyzer, dailycheck.Options{
		PaneLines:      int32(*paneLines),
		MaxPaneBytes:   *maxPaneBytes,
		DryRun:         *dryRun,
		PlatformProber: dailycheck.NewPlatformProber(),
		VerifyDelay:    750 * time.Millisecond,
	})
	if stateErr != nil {
		report.Problems = append(report.Problems, "loading notification state: "+stateErr.Error())
	}
	// A failure before Run could build a normal report (for example, the
	// daemon socket is unavailable) is itself an incident. Fold it into the
	// report before debouncing so it can never masquerade as recovery from a
	// previously reported session problem.
	if runErr != nil {
		recorded := false
		for _, problem := range report.Problems {
			if strings.HasPrefix(problem, "doctor escalation failed:") {
				recorded = true
				break
			}
		}
		if !recorded {
			report.Problems = append(report.Problems, "doctor run failed: "+runErr.Error())
		}
	}

	host, _ := os.Hostname()
	decision := dailycheck.DecideNotification(report, previousState)
	message := dailycheck.FormatNotification(host, report, runErr)
	if decision.Recovery {
		message = dailycheck.FormatRecoveryNotification(host, previousState)
	}
	fmt.Println(message)
	// Dry-run is genuinely side-effect free: proposed lifecycle repairs and
	// the Discord post are both suppressed, while the same report is printed.
	if !*dryRun {
		outbound := ""
		nextState := dailycheck.NotificationStateFor(report)
		if decision.Notify {
			outbound = message
		} else if previousState.PendingMessage != "" {
			outbound = previousState.PendingMessage
			nextState = previousState
		}
		if outbound != "" {
			notifyErr := sendDoctorNotification(identity.HomeDir, statePath, outbound, nextState)
			if notifyErr != nil {
				if runErr == nil {
					runErr = notifyErr
				} else {
					fmt.Fprintf(os.Stderr, "doctor: %v\n", notifyErr)
				}
			}
		}
	}
	if runErr != nil {
		log.Fatalf("doctor: %v", runErr)
	}
}

func sendDoctorNotification(home, statePath, message string, nextState dailycheck.NotificationState) error {
	path := discordnotify.PathForHome(home)
	cfg, err := discordnotify.Load(path)
	if err != nil {
		return fmt.Errorf("loading Discord config: %w", err)
	}
	if cfg.WebhookURL == "" {
		fmt.Fprintf(os.Stderr, "doctor: notable result, but Discord is not configured for this user (%s)\n", path)
		return nil
	}

	// Persist the outbound message before posting it. If Discord or the host
	// disappears mid-send, the next doctor run retries the incident instead of
	// silently losing an auto-recovered problem that is no longer reproducible.
	nextState.PendingMessage = message
	if err := dailycheck.SaveNotificationState(statePath, nextState); err != nil {
		return fmt.Errorf("saving pending Discord report: %w", err)
	}
	if err := discordnotify.Send(cfg.WebhookURL, message); err != nil {
		return fmt.Errorf("sending Discord report: %w", err)
	}
	nextState.PendingMessage = ""
	if err := dailycheck.SaveNotificationState(statePath, nextState); err != nil {
		return fmt.Errorf("clearing pending Discord report: %w", err)
	}
	return nil
}

func doctorIdentity(explicit string) (*user.User, error) {
	if explicit != "" {
		return user.Lookup(explicit)
	}
	if os.Geteuid() != 0 {
		return user.Current()
	}
	instances, err := discovery.List()
	if err != nil {
		return nil, fmt.Errorf("finding a Claude/Discord user: %w", err)
	}
	for _, instance := range instances {
		if instance.Agent == "claude-code" && instance.RunUser != "" {
			return user.Lookup(instance.RunUser)
		}
	}
	for _, instance := range instances {
		if instance.RunUser != "" {
			return user.Lookup(instance.RunUser)
		}
	}
	if len(instances) == 0 {
		return user.Current()
	}
	return nil, fmt.Errorf("could not infer a run user; create an instance or pass -run-user USER")
}
