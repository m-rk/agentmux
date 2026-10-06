package main

import (
	"context"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/ampsweep"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// sweepResult is one local sweep pass inside gc.
type sweepResult struct {
	host string
	res  ampsweep.Result
	err  error
}

// runLocalSweep runs one sweep pass over the run user's amp account and
// records what it archived for the gc pass. It shares its core with
// `amp sweep` (runAmpSweep): the same live-thread guard, the same
// rules, the same swept records. dryRun lists without archiving;
// runUser selects the account (empty auto-detects like doctor).
func runLocalSweep(dryRun bool, runUser string) sweepResult {
	identity, err := doctorIdentity(runUser)
	if err != nil {
		return sweepResult{err: err}
	}
	effectiveUser := ""
	if os.Geteuid() == 0 {
		effectiveUser = identity.Username
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	runner := &ampsweep.CLIRunner{RunUser: effectiveUser}
	now := time.Now()
	live := ampsweep.LiveThreads(ctx, discoveryInstances{}, func(ctx context.Context, src transcript.Source) ([]transcript.Thread, error) {
		r, err := transcript.For("amp")
		if err != nil {
			return nil, err
		}
		if effectiveUser != "" && src.RunUser == "" {
			src.RunUser = effectiveUser
			src.Home = identity.HomeDir
		}
		return r.Threads(ctx, src)
	})
	res, err := ampsweep.Sweep(ctx, runner, live, dryRun, now)
	if err != nil {
		return sweepResult{err: err}
	}
	if !dryRun && len(res.Candidates) > 0 {
		store := ampsweep.FileStore{Home: identity.HomeDir}
		for _, c := range res.Candidates {
			rec := ampsweep.SweptRecord{Thread: c.ID, Title: c.Title, Reason: c.Reason, ArchivedAt: now.UTC()}
			if serr := store.Save(rec); serr != nil {
				res.Warnings = append(res.Warnings, c.ID+": recording sweep: "+serr.Error())
			}
		}
	}
	return sweepResult{res: res}
}

// gcSweptPass deletes swept junk threads whose retention has expired.
// It runs inside the local gc pass (see ops.Env.GC via retire.GC):
// this wrapper resolves the identity and retention path the same way
// the sweep does, so both halves agree on whose records they read.
func gcSweptPass(ctx context.Context, dryRun bool, runUser string, now time.Time) (ampsweep.GCResult, error) {
	identity, err := doctorIdentity(runUser)
	if err != nil {
		return ampsweep.GCResult{}, err
	}
	effectiveUser := ""
	if os.Geteuid() == 0 {
		effectiveUser = identity.Username
	}
	runner := &ampsweep.CLIRunner{RunUser: effectiveUser}
	store := ampsweep.FileStore{Home: identity.HomeDir}
	retentionPath := identity.HomeDir + "/.config/agentmux/retention.yaml"
	return ampsweep.GC(ctx, store, runner, retentionPath, dryRun, now)
}
