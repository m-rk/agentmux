package retire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// fakeEnv is the test Env: canned registry, inspect state, and recorded
// effects. It never touches the host.
type fakeEnv struct {
	now      time.Time
	home     string
	registry map[string]map[string]string
	state    State
	inspect  error
	applied  []string
	managed  []string
	managedErr error
	deleted  []Record
	delErr   error
}

func testHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func (f *fakeEnv) Now() time.Time { return f.now }

func (f *fakeEnv) ReadRegistry(instance string) (map[string]string, error) {
	fields, ok := f.registry[instance]
	if !ok {
		return nil, errors.New("no such instance")
	}
	return fields, nil
}

func (f *fakeEnv) Home(string, map[string]string) string { return f.home }

func (f *fakeEnv) Inspect(context.Context, string, map[string]string) (State, error) {
	return f.state, f.inspect
}

func (f *fakeEnv) Apply(_ context.Context, instance, agent string, _ map[string]string, st State) (RetireResult, error) {
	f.applied = append(f.applied, instance+"/"+agent)
	res := RetireResult{Workdir: st.Workdir, Branch: st.Branch, Branches: st.Branches}
	for _, b := range st.Branches {
		if b.Deleted {
			res.BranchDeleted = true
		} else if b.Kept != "" && res.BranchKept == "" {
			res.BranchKept = b.Kept
		}
	}
	if agent == "amp" {
		res.AmpThreads = st.AmpThreads
	}
	if agent == "opencode" {
		res.OpencodeSessions = st.OpencodeSessions
	}
	return res, nil
}

func (f *fakeEnv) RemoveManaged(_ context.Context, instance string) (string, error) {
	f.managed = append(f.managed, instance)
	if f.managedErr != nil {
		return "", f.managedErr
	}
	return "stopped session, removed units and registry entry", nil
}

func (f *fakeEnv) GCHome() string { return f.home }

func (f *fakeEnv) RetentionPath() string {
	return filepath.Join(f.home, ".config", "agentmux", "retention.yaml")
}

func (f *fakeEnv) DeleteLeftovers(_ context.Context, rec Record) (GCDeleted, error) {
	if f.delErr != nil {
		return GCDeleted{}, f.delErr
	}
	f.deleted = append(f.deleted, rec)
	return deletedOf(rec), nil
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	return &fakeEnv{
		now:  time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
		home: testHome(t),
		registry: map[string]map[string]string{
			"task-1": {"AGENTMUX_AGENT": "amp", "AGENTMUX_WORKDIR": "/w/task-1"},
			"task-2": {"AGENTMUX_AGENT": "opencode", "AGENTMUX_WORKDIR": "/w/task-2"},
			"task-3": {"AGENTMUX_WORKDIR": "/w/task-3"}, // legacy: no agent field
			"web":    {"AGENTMUX_AGENT": "claude-code", "AGENTMUX_WORKDIR": "/w/web"},
		},
		state: State{Workdir: "/w/task-1", Branch: "task/1", Repo: "/repo",
			Branches:   []BranchFate{{Branch: "task/1", Deleted: true, Upstream: "origin/main"}},
			AmpThreads: []string{"T-00000000-0000-4000-8000-000000000001"}},
	}
}

func TestRetireRefusesNonTask(t *testing.T) {
	env := newFakeEnv(t)
	for _, name := range []string{"web", "mergentic", "agentmux", "task", "mytask-1"} {
		if _, err := Retire(context.Background(), env, name, Options{}); err == nil {
			t.Errorf("Retire(%q) = nil, want a refusal", name)
		} else if ReasonOf(err) != safesend.ReasonForbidden {
			t.Errorf("Retire(%q) reason = %s, want forbidden", name, ReasonOf(err))
		}
	}
	if len(env.applied) != 0 {
		t.Errorf("applied = %v, want nothing applied for non-task names", env.applied)
	}
}

func TestRetireRefusesUnknownInstance(t *testing.T) {
	env := newFakeEnv(t)
	if _, err := Retire(context.Background(), env, "task-nope", Options{}); ReasonOf(err) != safesend.ReasonNotFound {
		t.Errorf("reason = %s, want not_found", ReasonOf(err))
	}
}

func TestRetirePropagatesInspectError(t *testing.T) {
	env := newFakeEnv(t)
	env.inspect = errorf(safesend.ReasonInvalid, "worktree /w/task-1 has uncommitted changes; commit or stash them before retiring")
	_, err := Retire(context.Background(), env, "task-1", Options{})
	if ReasonOf(err) != safesend.ReasonInvalid {
		t.Fatalf("reason = %s, want invalid", ReasonOf(err))
	}
	if !strings.Contains(DetailOf(err), "uncommitted") {
		t.Errorf("detail = %q, want it to name uncommitted changes", DetailOf(err))
	}
	if len(env.applied) != 0 {
		t.Error("Apply ran despite the dirty worktree")
	}
}

func TestRetireDryRunChangesNothing(t *testing.T) {
	env := newFakeEnv(t)
	res, err := Retire(context.Background(), env, "task-1", Options{DryRun: true})
	if err != nil {
		t.Fatalf("Retire dry-run: %v", err)
	}
	if !res.DryRun || len(env.applied) != 0 {
		t.Errorf("dry-run applied changes: %+v applied=%v", res, env.applied)
	}
	if len(res.Plan) == 0 {
		t.Error("dry-run plan is empty")
	}
	if _, err := os.Stat(recordPath(env.home, "task-1")); !os.IsNotExist(err) {
		t.Error("dry-run wrote a retired record")
	}
}

func TestRetireWritesRecord(t *testing.T) {
	env := newFakeEnv(t)
	res, err := Retire(context.Background(), env, "task-1", Options{})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if len(res.AmpThreads) != 1 || res.AmpThreads[0] != "T-00000000-0000-4000-8000-000000000001" {
		t.Errorf("AmpThreads = %q", res.AmpThreads)
	}
	if !res.BranchDeleted || res.Branch != "task/1" {
		t.Errorf("branch = %+v", res)
	}
	if res.RetiredAt == "" {
		t.Error("RetiredAt is empty")
	}
	data, err := os.ReadFile(recordPath(env.home, "task-1"))
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parsing record: %v", err)
	}
	if rec.Agent != "amp" || len(rec.AmpThreads) != 1 || rec.Workdir != "/w/task-1" || rec.Branch != "task/1" {
		t.Errorf("record = %+v", rec)
	}
	if !rec.RetiredAt.Equal(env.now) {
		t.Errorf("RetiredAt = %v, want %v", rec.RetiredAt, env.now)
	}
}

func TestRetireLegacyAgentDefaultsToClaudeCode(t *testing.T) {
	env := newFakeEnv(t)
	env.state = State{Workdir: "/w/task-3", Branch: "task/3", Repo: "/repo", Branches: []BranchFate{{Branch: "task/3", Deleted: true}}}
	res, err := Retire(context.Background(), env, "task-3", Options{})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if res.Agent != "claude-code" {
		t.Errorf("Agent = %q, want claude-code for a registry without AGENTMUX_AGENT", res.Agent)
	}
	if len(env.applied) != 1 || env.applied[0] != "task-3/claude-code" {
		t.Errorf("applied = %v", env.applied)
	}
}

func TestRetireOpencodeRecordsSessions(t *testing.T) {
	env := newFakeEnv(t)
	env.state = State{Workdir: "/w/task-2", Branch: "task/2", Repo: "/repo",
		Branches:         []BranchFate{{Branch: "task/2", Deleted: true}},
		OpencodeSessions: []string{"ses-aaa", "ses-bbb"}}
	res, err := Retire(context.Background(), env, "task-2", Options{})
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if len(res.OpencodeSessions) != 2 {
		t.Errorf("OpencodeSessions = %v", res.OpencodeSessions)
	}
	data, err := os.ReadFile(recordPath(env.home, "task-2"))
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.OpencodeSessions) != 2 || len(rec.AmpThreads) != 0 {
		t.Errorf("record = %+v", rec)
	}
}

func saveTestRecord(t *testing.T, home string, rec Record) {
	t.Helper()
	if err := os.MkdirAll(stateDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath(home, rec.Instance), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGCDeletesOnlyDue(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	saveTestRecord(t, env.home, Record{Instance: "task-old", Agent: "amp",
		RetiredAt:  now.Add(-15 * 24 * time.Hour),
		AmpThreads: []string{"T-00000000-0000-4000-8000-000000000001"}})
	saveTestRecord(t, env.home, Record{Instance: "task-new", Agent: "opencode",
		RetiredAt: now.Add(-3 * 24 * time.Hour), OpencodeSessions: []string{"s1"}})
	saveTestRecord(t, env.home, Record{Instance: "web", Agent: "claude-code",
		RetiredAt: now.Add(-30 * 24 * time.Hour)})

	res, err := GC(context.Background(), env, false, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.RetentionDays != 14 {
		t.Errorf("RetentionDays = %d, want 14", res.RetentionDays)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Instance != "task-old" {
		t.Errorf("Deleted = %+v, want only task-old", res.Deleted)
	}
	if len(res.Kept) != 1 || res.Kept[0].Instance != "task-new" {
		t.Errorf("Kept = %+v, want only task-new", res.Kept)
	}
	if res.Kept[0].DeleteAt == "" {
		t.Error("Kept entry has no delete_at")
	}
	if len(env.deleted) != 1 || env.deleted[0].Instance != "task-old" {
		t.Errorf("deleted = %+v (the non-task record must never be touched)", env.deleted)
	}
	if _, err := os.Stat(recordPath(env.home, "task-old")); !os.IsNotExist(err) {
		t.Error("due record was not removed after deleting")
	}
	if _, err := os.Stat(recordPath(env.home, "task-new")); err != nil {
		t.Error("kept record was removed")
	}
}

func TestGCDryRunDeletesNothing(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	saveTestRecord(t, env.home, Record{Instance: "task-old", Agent: "amp",
		RetiredAt:  now.Add(-30 * 24 * time.Hour),
		AmpThreads: []string{"T-00000000-0000-4000-8000-000000000001"}})
	res, err := GC(context.Background(), env, true, now)
	if err != nil {
		t.Fatalf("GC dry-run: %v", err)
	}
	if !res.DryRun || len(res.Deleted) != 1 || len(env.deleted) != 0 {
		t.Errorf("dry-run deleted: %+v env.deleted=%v", res, env.deleted)
	}
	if _, err := os.Stat(recordPath(env.home, "task-old")); err != nil {
		t.Error("dry-run removed the record")
	}
}

func TestGCHonorsRetentionFile(t *testing.T) {
	env := newFakeEnv(t)
	now := env.now
	dir := filepath.Join(env.home, ".config", "agentmux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "retention.yaml"), []byte("retention_days: 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveTestRecord(t, env.home, Record{Instance: "task-old", Agent: "amp",
		RetiredAt: now.Add(-20 * 24 * time.Hour)})
	res, err := GC(context.Background(), env, false, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", res.RetentionDays)
	}
	if len(res.Deleted) != 0 || len(res.Kept) != 1 {
		t.Errorf("20-day-old record with 30-day retention: deleted=%v kept=%v", res.Deleted, res.Kept)
	}
}

func TestGCEmptyState(t *testing.T) {
	env := newFakeEnv(t)
	res, err := GC(context.Background(), env, false, env.now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.Deleted) != 0 || len(res.Kept) != 0 {
		t.Errorf("empty state: %+v", res)
	}
}

func TestPlan(t *testing.T) {
	st := State{Workdir: "/w/t", Branch: "task/9",
		Branches:   []BranchFate{{Branch: "task/9", Deleted: true, Upstream: "origin/main"}},
		AmpThreads: []string{"T-00000000-0000-4000-8000-000000000001"}}
	plan := st.Plan("amp")
	joined := strings.Join(plan, "\n")
	for _, want := range []string{"archive amp thread", "stop in-flight amp runs", "stop session", "remove units", "remove worktree",
		"delete branch task/9 (origin/main contains it)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("amp plan missing %q: %v", want, plan)
		}
	}
	// An unverified branch is kept with its reason — the AMUX-20
	// regression: the dry run must never claim "contains it" unchecked.
	kept := State{Workdir: "/w/t", Branch: "task/9",
		Branches: []BranchFate{{Branch: "task/9",
			Kept:     "branch task/9 has commits not on origin/main; merge it before retiring",
			Upstream: "origin/main"}}}.Plan("amp")
	if joined := strings.Join(kept, "\n"); !strings.Contains(joined, "keep branch task/9: branch task/9 has commits not on origin/main") {
		t.Errorf("unverified plan should keep the branch with a reason: %v", kept)
	}
	if strings.Contains(strings.Join(kept, "\n"), "delete branch") {
		t.Errorf("unverified plan claims a delete: %v", kept)
	}
	claude := State{Workdir: "/w/t", Branch: "task/9"}.Plan("claude-code")
	if strings.Join(claude, "\n") == "" || !strings.Contains(strings.Join(claude, "\n"), "keep transcripts") {
		t.Errorf("claude-code plan should name transcript keeping: %v", claude)
	}
	op := State{Workdir: "/w/t", Branch: "task/9", OpencodeSessions: []string{"a", "b"}}.Plan("opencode")
	if !strings.Contains(strings.Join(op, "\n"), "2 stored opencode sessions") {
		t.Errorf("opencode plan should count stored sessions: %v", op)
	}
	// A kept worktree is planned as kept with its reason.
	wt := State{Workdir: "/w/t", Branch: "task/9", WorktreeKept: "detached HEAD abc1234 is not on origin"}.Plan("amp")
	if joined := strings.Join(wt, "\n"); !strings.Contains(joined, "keep worktree /w/t: detached HEAD abc1234 is not on origin") {
		t.Errorf("plan should keep the worktree with a reason: %v", wt)
	}
	if strings.Contains(strings.Join(wt, "\n"), "remove worktree") {
		t.Errorf("plan claims the worktree would go: %v", wt)
	}
}
