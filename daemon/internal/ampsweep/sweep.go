package ampsweep

import (
	"context"
	"strings"
	"time"
)

// Thresholds for the junk rules. A thread younger than the rule's age, or
// with more than MaxMessages messages, is never touched by that rule —
// archiving is reversible, but a false positive still hides somebody's
// live thread from them for a day.
const (
	// MaxMessages is the hard ceiling: no rule ever archives a thread
	// with more messages. Every junk class below is small by nature
	// (strays, probes, failed starts, one-shot reviews).
	MaxMessages = 6
	// ProbeAge is how long a short ordinary thread must sit idle before
	// it counts as an abandoned probe ("ok", "reply with exactly ...").
	ProbeAge = 6 * time.Hour
	// RelayAge is how long a relay stray must sit before archiving. The
	// "[relayed by ...]" prefix already proves it never belonged to a
	// task instance, so the wait only guards against racing a send that
	// just happened; the daily sweep re-sees anything it skips today.
	RelayAge = time.Hour
	// ErrorAge bounds the untitled-error rule the same way.
	ErrorAge = time.Hour
	// ReviewAge keeps the last few nightly reviews around for the
	// operator to read, then archives them.
	ReviewAge = 72 * time.Hour
)

// Prefixes matching the first message of special thread classes:
//
//   - relayedPrefix / sentByPrefix come from safesend.Provenance.Prefix
//     ("[relayed by orchestrator]", "[sent by ...]"): a sessions send
//     artifact that never reached a live worker — a stray owned by no
//     instance, so no retire or gc step could ever reach it.
//   - dispatchedPrefix is written by the orchestrator when it dispatches
//     a task worker through sessions run ("[dispatched by mergentic
//     from AMUX-44] ..."). A dispatched worker belongs to a task
//     instance even when amp's auto-title replaced its sidebar title,
//     so the sweep never archives one: a dead task's worker is retire's
//     to archive, not the sweep's.
//   - reviewPrefix is the nightly reviewer's opening line (see
//     threadwatch's reviewSystemPrompt): the nightly review thread,
//     labeled agentmux-review, which would otherwise look like a probe.
const (
	relayedPrefix    = "[relayed by "
	sentByPrefix     = "[sent by "
	dispatchedPrefix = "[dispatched by "
	reviewPrefix     = "You are agentmux's nightly reviewer."
)

// ListedThread is one `amp threads list --json` row: the cheap signal
// used to decide which threads deserve a full export. MessageCount is
// the per-row count the CLI reports; it undercounts (it showed 4 for a
// thread whose export holds 10), so Sweep trusts it only to skip the
// export of a probably-big thread, never to archive one.
type ListedThread struct {
	ID           string
	Title        string
	Updated      time.Time
	MessageCount int
}

// ExportedThread is the part of `amp threads export` the rules need:
// the first user message's text (prefix matching), the agent state
// (error detection), and the true message count (the list row's count
// undercounts: it showed 4 for a thread whose export holds 10).
type ExportedThread struct {
	ID        string
	Title     string
	FirstText string
	State     string
	Messages  int
	Updated   time.Time
}

// Runner runs amp for the sweep. The CLI command wires CLIRunner; tests
// substitute a fake. Delete removes a thread swept on an earlier run
// whose retention has expired (`amp threads delete`).
type Runner interface {
	List(ctx context.Context) ([]ListedThread, error)
	Export(ctx context.Context, id string) (*ExportedThread, error)
	Archive(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
}

// Candidate is one thread the sweep archived (or would archive on a dry
// run), with the human-readable reason printed for each.
type Candidate struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason"`
}

// Result is what a sweep found and did.
type Result struct {
	// Examined counts listed threads; Kept counts listed threads left
	// alone (live, young, big, or simply not junk).
	Examined int `json:"examined"`
	Kept     int `json:"kept"`
	// Candidates are the archived threads (dry run: what would go).
	Candidates []Candidate `json:"candidates,omitempty"`
	// Warnings names threads whose export or archive failed: they are
	// left alone and re-seen by the next run.
	Warnings []string `json:"warnings,omitempty"`
	DryRun   bool     `json:"dry_run,omitempty"`
}

// Sweep lists the account's live threads and archives the junk ones (see
// the package doc comment). live holds the recorded thread ids of live
// task instances — a dispatched worker of a live task is never touched,
// even if it somehow looks like junk. dryRun lists without archiving.
// A failed export or archive warns and continues; only a failed list
// fails the whole sweep.
func Sweep(ctx context.Context, r Runner, live map[string]bool, dryRun bool, now time.Time) (Result, error) {
	res := Result{DryRun: dryRun}
	listed, err := r.List(ctx)
	if err != nil {
		return Result{}, err
	}
	for _, t := range listed {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if t.ID == "" || live[t.ID] {
			res.Examined++
			res.Kept++
			continue
		}
		// An empty title means the thread has no sidebar title (the CLI
		// prints "<ID> <title>", so a titled thread always shows one):
		// it still needs its state checked for the error rule, which
		// is why the ceiling skip below exempts untitled threads from
		// the export skip.
		if t.MessageCount > MaxMessages && strings.TrimSpace(t.Title) != "" {
			res.Examined++
			res.Kept++
			continue
		}
		ex, err := r.Export(ctx, t.ID)
		if err != nil {
			res.Examined++
			res.Kept++
			res.Warnings = append(res.Warnings, t.ID+": export failed ("+shortErr(err)+")")
			continue
		}
		res.Examined++
		reason, keep := classify(ex, live, now)
		if keep {
			res.Kept++
			continue
		}
		if !dryRun {
			if err := r.Archive(ctx, ex.ID); err != nil {
				res.Kept++
				res.Warnings = append(res.Warnings, ex.ID+": archive failed ("+shortErr(err)+")")
				continue
			}
		}
		res.Candidates = append(res.Candidates, Candidate{ID: ex.ID, Title: ex.Title, Reason: reason})
	}
	return res, nil
}

// classify applies the junk rules to one exported thread: the reason to
// archive it, or keep=true to leave it alone. The order matters: the
// live-worker and dispatched-worker guards run before any archive rule,
// and each archive rule returns either way so a matched thread never
// falls through to a looser rule (a young relay stray is kept, not
// re-examined as a probe).
func classify(ex *ExportedThread, live map[string]bool, now time.Time) (reason string, keep bool) {
	if live[ex.ID] {
		return "", true
	}
	if ex.Messages > MaxMessages {
		return "", true
	}
	age := now.Sub(ex.Updated)
	if ex.Updated.IsZero() {
		// No timestamp means the age cannot be proven: keep.
		return "", true
	}
	first := ex.FirstText
	switch {
	case strings.HasPrefix(first, dispatchedPrefix):
		// A dispatched worker belongs to a task instance — retire
		// archives it with the instance, never the sweep.
		return "", true
	case strings.HasPrefix(first, reviewPrefix):
		if age >= ReviewAge {
			return "nightly review older than 3 days", false
		}
		return "", true
	case strings.HasPrefix(first, relayedPrefix) || strings.HasPrefix(first, sentByPrefix):
		if age >= RelayAge {
			return "relay stray, owned by no instance", false
		}
		return "", true
	}
	if strings.TrimSpace(ex.Title) == "" && ex.State == "error" {
		if age >= ErrorAge {
			return "untitled thread in error state", false
		}
		return "", true
	}
	if age >= ProbeAge {
		return "short idle thread", false
	}
	return "", true
}

// shortErr trims an export/archive failure to one line for the warning.
func shortErr(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}
