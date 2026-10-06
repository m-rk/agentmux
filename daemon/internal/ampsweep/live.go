// Live worker threads: the sweep must never archive a dispatched worker
// of a live task instance, even when amp's auto-title replaced its
// sidebar title. Detection is two-layered:
//
//  1. Registry: every amp instance's recorded thread ids, from the
//     `sessions run` state dir (~/.local/state/agentmux/sessions/
//     <instance>/amp-run-<thread>.jsonl — see retire.recordedAmpThreads)
//     and from the runner mapping (transcript.Threads). Cheap, on-disk,
//     no amp export needed — and it finds exactly the threads retire
//     would archive with the instance.
//
//  2. Export prefix: a thread whose first message starts "[dispatched
//     by " belongs to a task instance (see sweep.go). Fresh instances
//     may not have recorded ids yet; long-dead instances left junk
//     behind whose prefix still matches — layer 1 decides those:
//
//     - a dispatched thread recorded for a live task instance is kept:
//     the worker may still be running;
//     - a dispatched thread recorded for no live task instance (an
//     abandoned worker whose task is over but was never retired) is
//     also kept: that thread is retire's to archive with a manual
//     retire of the finished task, not the sweep's to guess at.
//
// Either way the sweep never archives a dispatched worker; prefix
// detection only matters where the runner id comes from.
package ampsweep

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// Instances lists this host's task instances for LiveThreads. The CLI
// command wires discovery; tests substitute a fake.
type Instances interface {
	// Live lists the live task instances: name, agent, runner id, and
	// home dir for the recorded-thread state dir.
	Live(ctx context.Context) ([]LiveInstance, error)
}

// LiveInstance is one live task instance the sweep must protect.
type LiveInstance struct {
	Name   string
	Runner string
	Home   string
	// RunUser is the instance's AGENTMUX_RUN_USER; the transcript
	// reader runs amp as this user. Empty means the current user.
	RunUser string
	Workdir string
}

// LiveThreads returns the recorded thread ids of live amp task
// instances: the runner's own threads (through the transcript reader,
// which maps threads to the runner) plus the `sessions run` recorded
// ids (which retire itself records, so the sets agree). Only task-*
// amp instances are read — anything else is not a dispatched worker.
func LiveThreads(ctx context.Context, inst Instances, threads func(context.Context, transcript.Source) ([]transcript.Thread, error)) map[string]bool {
	live := map[string]bool{}
	instances, err := inst.Live(ctx)
	if err != nil {
		return live
	}
	for _, in := range instances {
		if !strings.HasPrefix(in.Name, "task-") {
			continue
		}
		for _, id := range recordedThreads(in.Home, in.Name) {
			live[id] = true
		}
		src := transcript.Source{Instance: in.Name, Agent: "amp", Workdir: in.Workdir, Home: in.Home, RunUser: in.RunUser, AmpRunnerID: in.Runner}
		ts, err := threads(ctx, src)
		if err != nil {
			continue
		}
		for _, t := range ts {
			live[t.ID] = true
		}
	}
	return live
}

// recordedThreads lists the thread ids in the instance's run-log state
// dir, newest first — the same logs retire.recordedAmpThreads reads, so
// the sweep and retire agree on which threads belong to the instance.
// An empty or missing dir is nil, not an error.
func recordedThreads(home, instance string) []string {
	if home == "" || instance == "" {
		return nil
	}
	dir := filepath.Join(home, ".local", "state", "agentmux", "sessions", instance)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type logged struct {
		id  string
		mod time.Time
	}
	var found []logged
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "amp-run-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "amp-run-"), ".jsonl")
		if id == "pending" || !transcript.ValidAmpThreadID(id) {
			continue
		}
		mod := time.Time{}
		if info, err := e.Info(); err == nil {
			mod = info.ModTime()
		}
		found = append(found, logged{id: id, mod: mod})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	var ids []string
	for _, f := range found {
		ids = append(ids, f.id)
	}
	return ids
}
