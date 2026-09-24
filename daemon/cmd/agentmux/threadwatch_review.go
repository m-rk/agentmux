package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/threadwatch"
)

// defaultThreadwatchReviewBin is the binary path an installed review timer
// execs, matching the stable path `agentmux daemon install` copies itself
// to (internal/daemoninstall's unexported binPath). There is no exported
// getter for that constant, so it is duplicated here rather than adding
// one just for this.
const defaultThreadwatchReviewBin = "/usr/local/bin/agentmux"

// runThreadwatchReview is `agentmux threadwatch review`: the nightly
// aggregation + digest job (docs/design/thread-watch.md, "Nightly review
// and digest"), and its `install` subcommand that sets up the systemd
// timer running it. It is dispatched from threadwatch_cmd.go.
func runThreadwatchReview(args []string) {
	if len(args) > 0 && args[0] == "install" {
		runThreadwatchReviewInstall(args[1:])
		return
	}

	fs := flag.NewFlagSet("threadwatch review", flag.ExitOnError)
	since := fs.Duration("since", 24*time.Hour, "how far back to aggregate events/signals")
	runUser := fs.String("run-user", "", "OS user whose threadwatch state, Claude login, and Discord webhook to use (root jobs default like doctor)")
	model := fs.String("model", "", "optional Claude model override (empty uses the user's Claude default)")
	dryRun := fs.Bool("dry-run", false, "print the digest; skip Discord and the report file")
	noModel := fs.Bool("no-model", false, "skip the Claude review; build the digest from stats and cluster titles only")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall review timeout")
	weeklyQuiet := fs.Bool("weekly-quiet", false, `on Sundays, if nothing is notable, send a one-line "all quiet" summary instead of nothing`)
	fs.Parse(args)

	identity, err := doctorIdentity(*runUser)
	if err != nil {
		log.Fatalf("threadwatch review: %v", err)
	}
	home := identity.HomeDir

	cfg, err := threadwatch.LoadConfig(threadwatch.DefaultConfigPath(home))
	if err != nil {
		log.Fatalf("threadwatch review: %v", err)
	}

	store, err := threadwatch.NewStore(threadwatch.StateDir(home))
	if err != nil {
		log.Fatalf("threadwatch review: opening state store: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	until := time.Now()
	sinceTime := until.Add(-*since)
	events, err := store.ReadEvents(sinceTime, until)
	if err != nil {
		log.Fatalf("threadwatch review: reading events: %v", err)
	}
	signals, err := store.ReadSignals(sinceTime, until)
	if err != nil {
		log.Fatalf("threadwatch review: reading signals: %v", err)
	}

	input := threadwatch.BuildReview(events, signals, cfg, sinceTime, until)

	result, modelFailed := reviewResult(ctx, input, identity, home, *model, *noModel)

	host, _ := os.Hostname()
	digest := threadwatch.FormatDigest(host, input, result)
	if modelFailed {
		digest = prependWarning(digest, "⚠️ review model failed; falling back to stats-only insights")
	}
	report := threadwatch.FormatReport(host, input, result)

	fmt.Println(digest)

	if *dryRun {
		return
	}

	reviewDir := threadwatch.ReviewDir(home)
	if err := os.MkdirAll(reviewDir, 0o700); err != nil {
		log.Fatalf("threadwatch review: creating %s: %v", reviewDir, err)
	}
	reportPath := filepath.Join(reviewDir, until.Format("2006-01-02")+".md")
	if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
		log.Fatalf("threadwatch review: writing %s: %v", reportPath, err)
	}

	outbound := ""
	switch {
	case threadwatch.Notable(result):
		outbound = digest
	case *weeklyQuiet && until.Weekday() == time.Sunday:
		outbound = threadwatch.FormatAllQuiet(host, input)
	}
	if outbound == "" {
		return
	}

	discordPath := discordnotify.PathForHome(home)
	discordCfg, err := discordnotify.Load(discordPath)
	if err != nil {
		log.Fatalf("threadwatch review: loading Discord config: %v", err)
	}
	if discordCfg.WebhookURL == "" {
		fmt.Fprintf(os.Stderr, "threadwatch review: notable result, but Discord is not configured for this user (%s)\n", discordPath)
		return
	}
	if err := discordnotify.Send(discordCfg.WebhookURL, outbound); err != nil {
		log.Fatalf("threadwatch review: sending Discord digest: %v", err)
	}
}

// reviewResult runs the bounded Claude escalation unless noModel is set,
// falling back to FallbackReview either way it can't run: -no-model itself,
// or any failure from Claude (network, auth, a bad reply — the design doc's
// "never alert less" principle applies here too: a broken model step must
// not silence the review). The second return reports whether Claude was
// attempted and failed, so the caller can flag it in the digest.
func reviewResult(ctx context.Context, input threadwatch.ReviewInput, identity *user.User, home, model string, noModel bool) (threadwatch.ReviewResult, bool) {
	if noModel {
		return threadwatch.FallbackReview(input), false
	}

	command := threadwatch.CommandFactory(runas.CurrentUserCommandContext)
	if os.Geteuid() == 0 {
		command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return runas.CommandContext(ctx, identity.Username, name, args...)
		}
	}
	reviewer := threadwatch.ClaudeReviewer{Command: command, Model: model, Dir: home}
	result, err := reviewer.Review(ctx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "threadwatch review: Claude review failed, falling back to stats only: %v\n", err)
		return threadwatch.FallbackReview(input), true
	}
	return result, false
}

// prependWarning adds a warning line ahead of an already-formatted (and
// already reviewDigestCap-bounded) digest, re-truncating on a UTF-8
// boundary if the combined message would exceed the same cap FormatDigest
// uses.
func prependWarning(digest, warning string) string {
	combined := warning + "\n" + digest
	const limit = 1900 // matches threadwatch.reviewDigestCap
	if len(combined) <= limit {
		return combined
	}
	cut := limit
	for cut > 0 && (combined[cut]&0xC0) == 0x80 {
		cut--
	}
	return combined[:cut] + "…"
}

func runThreadwatchReviewInstall(args []string) {
	fs := flag.NewFlagSet("threadwatch review install", flag.ExitOnError)
	at := fs.String("at", "07:00", "local time (HH:MM) to run the nightly review, after the doctor")
	runUser := fs.String("run-user", "", "OS user to run the review as (default: auto-detected like doctor)")
	bin := fs.String("bin", defaultThreadwatchReviewBin, "agentmux binary path the installed unit execs")
	print := fs.Bool("print", false, "print the systemd units without installing them")
	fs.Parse(args)

	identity, err := doctorIdentity(*runUser)
	if err != nil {
		log.Fatalf("threadwatch review install: %v", err)
	}

	if err := installThreadwatchReviewTimer(identity.Username, *bin, *at, *print); err != nil {
		log.Fatalf("threadwatch review install: %v", err)
	}
}
