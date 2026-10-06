package ampsweep

import (
	"context"
	"sort"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/retention"
)

// SweptRecord is one swept thread the sweep recorded for a later gc:
// the thread id, when it was archived, and the reason. Stored as
// ~/.local/state/agentmux/swept/<thread>.json under the run user's
// home (see store.go's sibling retired records in retire/state.go).
type SweptRecord struct {
	// Thread is the archived amp thread id.
	Thread string `json:"thread"`
	// Title is the thread's title at sweep time, for the report.
	Title string `json:"title,omitempty"`
	// Reason is the sweep's human-readable archive reason.
	Reason string `json:"reason,omitempty"`
	// ArchivedAt is when the sweep archived it (UTC); the sweep's own
	// gc counts its retention from here.
	ArchivedAt time.Time `json:"archived_at"`
}

// GCResult is what the swept-thread gc did, for the CLI report.
type GCResult struct {
	// RetentionDays is the retention that applied.
	RetentionDays int `json:"retention_days"`
	// Deleted lists swept threads deleted after retention.
	Deleted []string `json:"deleted,omitempty"`
	// Kept lists swept threads still inside retention.
	Kept []SweptKept `json:"kept,omitempty"`
	// Missing lists swept records whose thread is already gone from
	// the account (deleted by hand, or by an earlier gc): the records
	// are dropped, not kept forever.
	Missing []string `json:"missing,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// SweptKept is one swept thread still inside retention.
type SweptKept struct {
	Thread   string `json:"thread"`
	DeleteAt string `json:"delete_at,omitempty"`
}

// Store persists swept records across runs. The CLI command wires
// FileStore; tests substitute a fake.
type Store interface {
	// Save records one swept thread.
	Save(rec SweptRecord) error
	// Load returns every swept record.
	Load() ([]SweptRecord, error)
	// Remove drops one swept record.
	Remove(thread string) error
}

// GC deletes swept threads archived longer ago than the host retention
// (retention.yaml, default 14 days — the same retention retire's gc
// enforces, so archived junk of either kind ages out together). A
// swept thread whose record is due is deleted and its record dropped;
// a thread already gone from the account drops its record without a
// delete. dryRun lists without deleting anything.
func GC(ctx context.Context, store Store, r Runner, retentionPath string, dryRun bool, now time.Time) (GCResult, error) {
	days := retentionDays(retentionPath)
	res := GCResult{RetentionDays: days, DryRun: dryRun}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	recs, err := store.Load()
	if err != nil {
		return GCResult{}, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Thread < recs[j].Thread })
	dueAfter := time.Duration(days) * 24 * time.Hour
	for _, rec := range recs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if now.Sub(rec.ArchivedAt) < dueAfter {
			res.Kept = append(res.Kept, SweptKept{Thread: rec.Thread,
				DeleteAt: rec.ArchivedAt.Add(dueAfter).Format(time.RFC3339)})
			continue
		}
		if dryRun {
			res.Deleted = append(res.Deleted, rec.Thread)
			continue
		}
		gone, err := isGone(ctx, r, rec.Thread)
		if err != nil {
			return res, err
		}
		if !gone {
			if err := r.Delete(ctx, rec.Thread); err != nil {
				return res, err
			}
		}
		if err := store.Remove(rec.Thread); err != nil {
			return res, err
		}
		if gone {
			res.Missing = append(res.Missing, rec.Thread)
		} else {
			res.Deleted = append(res.Deleted, rec.Thread)
		}
	}
	return res, nil
}

// isGone reports whether the thread is already gone from the account:
// an export failure means archived-gone (a visible thread exports
// fine), so the record drops instead of blocking the gc forever. A
// cancelled context is not "gone" — it aborts the gc.
func isGone(ctx context.Context, r Runner, thread string) (bool, error) {
	if _, err := r.Export(ctx, thread); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, nil
	}
	return false, nil
}

// retentionDays loads the host retention, defaulting on any problem the
// same way retire does: a broken host file is caller-visible only when
// it would matter, and gc always has a sane number to enforce.
func retentionDays(path string) int {
	cfg, err := retention.Load(path)
	if err != nil {
		return retention.DefaultDays
	}
	return cfg.RetentionDays
}
