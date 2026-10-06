package ampsweep

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRunner is the test Runner: canned list/exports, recorded archives
// and deletes, injectable failures. It never touches the host.
type fakeRunner struct {
	listed    []ListedThread
	exports   map[string]*ExportedThread
	exportErr map[string]error
	archErr   map[string]error
	archived  []string
	deleted   []string
	delErr    error
}

func (f *fakeRunner) List(context.Context) ([]ListedThread, error) {
	return f.listed, nil
}

func (f *fakeRunner) Export(_ context.Context, id string) (*ExportedThread, error) {
	if err, ok := f.exportErr[id]; ok {
		return nil, err
	}
	if ex, ok := f.exports[id]; ok {
		return ex, nil
	}
	return nil, errors.New("fake amp: no such thread " + id)
}

func (f *fakeRunner) Archive(_ context.Context, id string) error {
	if err, ok := f.archErr[id]; ok {
		return err
	}
	f.archived = append(f.archived, id)
	return nil
}

func (f *fakeRunner) Delete(_ context.Context, id string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

var sweepNow = time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)

func listed(id, title string, age time.Duration, count int) ListedThread {
	return ListedThread{ID: id, Title: title, Updated: sweepNow.Add(-age), MessageCount: count}
}

func exported(id, title, first, state string, age time.Duration, msgs int) *ExportedThread {
	return &ExportedThread{ID: id, Title: title, FirstText: first, State: state, Messages: msgs, Updated: sweepNow.Add(-age)}
}

func sweepIDs(res Result) []string {
	var ids []string
	for _, c := range res.Candidates {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestSweepArchivesRelayStray(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000001"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "relay", 2*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "relay", "[relayed by orchestrator]\ncarry on", "idle", 2*time.Hour, 2)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := sweepIDs(res); len(got) != 1 || got[0] != id {
		t.Fatalf("candidates = %v, want [%s]", got, id)
	}
	if len(f.archived) != 1 || f.archived[0] != id {
		t.Fatalf("archived = %v", f.archived)
	}
	want := "relay stray, owned by no instance"
	if res.Candidates[0].Reason != want {
		t.Errorf("reason = %q, want %q", res.Candidates[0].Reason, want)
	}
}

func TestSweepKeepsYoungRelayStray(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000001"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "relay", 30*time.Minute, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "relay", "[relayed by orchestrator]\ncarry on", "idle", 30*time.Minute, 2)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || len(f.archived) != 0 {
		t.Fatalf("young relay stray archived: %+v", res)
	}
	if res.Kept != 1 {
		t.Errorf("kept = %d, want 1", res.Kept)
	}
}

func TestSweepKeepsDispatchedWorker(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000002"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "Host mode enforcement", 48*time.Hour, 2)},
		exports: map[string]*ExportedThread{id: exported(id, "Host mode enforcement", "[dispatched by mergentic from AMUX-36]\nTask AMUX-36: ...", "idle", 48*time.Hour, 2)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || len(f.archived) != 0 {
		t.Fatalf("dispatched worker archived: %+v", res)
	}
}

func TestSweepKeepsLiveRecordedThread(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000003"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "ok", 48*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "ok", "ok", "idle", 48*time.Hour, 1)},
	}
	live := map[string]bool{id: true}
	res, err := Sweep(context.Background(), f, live, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || len(f.archived) != 0 {
		t.Fatalf("live recorded thread archived: %+v", res)
	}
}

func TestSweepArchivesOldProbe(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000004"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "MODE-CHECK", 30*time.Hour, 4)},
		exports: map[string]*ExportedThread{id: exported(id, "MODE-CHECK", "reply with exactly: MODE-CHECK", "idle", 30*time.Hour, 4)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := sweepIDs(res); len(got) != 1 || got[0] != id {
		t.Fatalf("candidates = %v, want [%s]", got, id)
	}
	if res.Candidates[0].Reason != "short idle thread" {
		t.Errorf("reason = %q", res.Candidates[0].Reason)
	}
}

func TestSweepKeepsYoungProbe(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000005"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "probe", 2*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "probe", "ok", "idle", 2*time.Hour, 1)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("young probe archived: %+v", res)
	}
}

func TestSweepArchivesUntitledError(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000006"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "", 2*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "", "do the thing", "error", 2*time.Hour, 1)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := sweepIDs(res); len(got) != 1 || got[0] != id {
		t.Fatalf("candidates = %v, want [%s]", got, id)
	}
	if res.Candidates[0].Reason != "untitled thread in error state" {
		t.Errorf("reason = %q", res.Candidates[0].Reason)
	}
}

func TestSweepKeepsTitledError(t *testing.T) {
	// A titled thread in error state is somebody's thread that failed,
	// not junk: only the untitled-error rule may archive errors.
	id := "T-00000000-0000-4000-8000-000000000007"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "my work", 2*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "my work", "do the thing", "error", 2*time.Hour, 1)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("titled error thread archived: %+v", res)
	}
}

func TestSweepNeverTouchesBigThread(t *testing.T) {
	// Even a relay-prefixed thread is kept past the message ceiling:
	// more than 6 messages means real conversation happened.
	id := "T-00000000-0000-4000-8000-000000000008"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "relay", 72*time.Hour, 3)},
		exports: map[string]*ExportedThread{id: exported(id, "relay", "[relayed by orchestrator]\ncarry on", "idle", 72*time.Hour, 7)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || len(f.archived) != 0 {
		t.Fatalf("big thread archived: %+v", res)
	}
}

func TestSweepSkipsExportPastListCeiling(t *testing.T) {
	// The listed count is trusted to skip the export (a probable miss,
	// never a wrong archive) — except an untitled thread, which still
	// needs its state checked for the error rule.
	id := "T-00000000-0000-4000-8000-000000000009"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "busy", 72*time.Hour, 96)},
		exports: map[string]*ExportedThread{},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || res.Kept != 1 {
		t.Fatalf("big listed thread not kept: %+v", res)
	}
}

func TestSweepArchivesOldReview(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000010"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "Nightly session review insights", 4*24*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "Nightly session review insights", "You are agentmux's nightly reviewer. You receive ...", "idle", 4*24*time.Hour, 2)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := sweepIDs(res); len(got) != 1 || got[0] != id {
		t.Fatalf("candidates = %v, want [%s]", got, id)
	}
	if res.Candidates[0].Reason != "nightly review older than 3 days" {
		t.Errorf("reason = %q", res.Candidates[0].Reason)
	}
}

func TestSweepKeepsFreshReview(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000011"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "Nightly session review insights", 2*24*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "Nightly session review insights", "You are agentmux's nightly reviewer. You receive ...", "idle", 2*24*time.Hour, 2)},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("fresh review archived: %+v", res)
	}
}

func TestSweepDryRunArchivesNothing(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000012"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "relay", 2*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "relay", "[sent by mark]\nhello?", "idle", 2*time.Hour, 2)},
	}
	res, err := Sweep(context.Background(), f, nil, true, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want the stray", res)
	}
	if !res.DryRun {
		t.Error("DryRun not set on the result")
	}
	if len(f.archived) != 0 {
		t.Fatalf("dry run archived: %v", f.archived)
	}
}

func TestSweepExportFailureWarnsAndKeeps(t *testing.T) {
	good := "T-00000000-0000-4000-8000-000000000013"
	bad := "T-00000000-0000-4000-8000-000000000014"
	f := &fakeRunner{
		listed: []ListedThread{
			listed(good, "relay", 2*time.Hour, 1),
			listed(bad, "relay", 2*time.Hour, 1),
		},
		exports:   map[string]*ExportedThread{good: exported(good, "relay", "[relayed by x]\nyo", "idle", 2*time.Hour, 1)},
		exportErr: map[string]error{bad: errors.New("amp threads export: boom")},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := sweepIDs(res); len(got) != 1 || got[0] != good {
		t.Fatalf("candidates = %v, want [%s]", got, good)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], bad) {
		t.Fatalf("warnings = %v, want the failed export", res.Warnings)
	}
	if res.Kept != 1 {
		t.Errorf("kept = %d, want 1 (the failed export)", res.Kept)
	}
}

func TestSweepArchiveFailureWarnsAndKeeps(t *testing.T) {
	id := "T-00000000-0000-4000-8000-000000000015"
	f := &fakeRunner{
		listed:  []ListedThread{listed(id, "relay", 2*time.Hour, 1)},
		exports: map[string]*ExportedThread{id: exported(id, "relay", "[relayed by x]\nyo", "idle", 2*time.Hour, 1)},
		archErr: map[string]error{id: errors.New("amp threads archive: boom")},
	}
	res, err := Sweep(context.Background(), f, nil, false, sweepNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("failed archive listed as candidate: %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], id) {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}
