package threadwatch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultPaths(t *testing.T) {
	home := "/home/user"
	if got, want := DefaultConfigPath(home), "/home/user/.config/agentmux/threadwatch.yaml"; got != want {
		t.Errorf("DefaultConfigPath = %q, want %q", got, want)
	}
	if got, want := StateDir(home), "/home/user/.local/state/agentmux/threadwatch"; got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
	if got, want := ReviewDir(home), "/home/user/.local/state/agentmux/reviews"; got != want {
		t.Errorf("ReviewDir = %q, want %q", got, want)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "nope.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg, DefaultConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("LoadConfig on missing file = %+v, want DefaultConfig() %+v", got, want)
	}
}

func TestLoadConfigOverridesKeepOtherDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threadwatch.yaml")
	yamlSrc := `
thresholds:
  awaiting_user_after: 5m
  api_error_loop: 2
alerts:
  cooldown: 1800
jev:
  mode: live
  page_urgency: 4.5
instances:
  myinstance:
    disabled: true
    jev: false
    thresholds:
      stalled_turn_after: 20m
`
	if err := os.WriteFile(path, []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	def := DefaultConfig()

	if cfg.Thresholds.AwaitingUserAfter != 5*time.Minute {
		t.Errorf("AwaitingUserAfter = %v, want 5m", cfg.Thresholds.AwaitingUserAfter)
	}
	if cfg.Thresholds.APIErrorLoop != 2 {
		t.Errorf("APIErrorLoop = %v, want 2", cfg.Thresholds.APIErrorLoop)
	}
	// Unset threshold fields keep the default.
	if cfg.Thresholds.StalledTurnAfter != def.Thresholds.StalledTurnAfter {
		t.Errorf("StalledTurnAfter = %v, want default %v", cfg.Thresholds.StalledTurnAfter, def.Thresholds.StalledTurnAfter)
	}
	if cfg.Thresholds.ToolErrorLoop != def.Thresholds.ToolErrorLoop {
		t.Errorf("ToolErrorLoop = %v, want default %v", cfg.Thresholds.ToolErrorLoop, def.Thresholds.ToolErrorLoop)
	}

	// Bare integer duration is seconds.
	if cfg.Alerts.Cooldown != 1800*time.Second {
		t.Errorf("Cooldown = %v, want 1800s", cfg.Alerts.Cooldown)
	}
	if cfg.Alerts.MaxPerHour != def.Alerts.MaxPerHour {
		t.Errorf("MaxPerHour = %v, want default %v", cfg.Alerts.MaxPerHour, def.Alerts.MaxPerHour)
	}

	if cfg.Jev.Mode != "live" {
		t.Errorf("Jev.Mode = %q, want live", cfg.Jev.Mode)
	}
	if cfg.Jev.PageUrgency != 4.5 {
		t.Errorf("Jev.PageUrgency = %v, want 4.5", cfg.Jev.PageUrgency)
	}
	if cfg.Jev.Model != def.Jev.Model {
		t.Errorf("Jev.Model = %q, want default %q", cfg.Jev.Model, def.Jev.Model)
	}

	ic, ok := cfg.Instances["myinstance"]
	if !ok {
		t.Fatal("missing instance override")
	}
	if !ic.Disabled {
		t.Error("Disabled = false, want true")
	}
	if ic.Jev == nil || *ic.Jev {
		t.Errorf("Jev = %v, want pointer to false", ic.Jev)
	}
	if ic.Review != nil {
		t.Errorf("Review = %v, want nil", ic.Review)
	}
	if ic.Thresholds == nil || ic.Thresholds.StalledTurnAfter != 20*time.Minute {
		t.Fatalf("instance Thresholds = %+v", ic.Thresholds)
	}
	// Unset fields on an instance override stay zero (no override), not
	// the package default: ThresholdsFor relies on this.
	if ic.Thresholds.AwaitingUserAfter != 0 {
		t.Errorf("instance AwaitingUserAfter = %v, want 0 (no override)", ic.Thresholds.AwaitingUserAfter)
	}

	effective := cfg.ThresholdsFor("myinstance")
	if effective.StalledTurnAfter != 20*time.Minute {
		t.Errorf("ThresholdsFor StalledTurnAfter = %v, want 20m", effective.StalledTurnAfter)
	}
	if effective.AwaitingUserAfter != cfg.Thresholds.AwaitingUserAfter {
		t.Errorf("ThresholdsFor AwaitingUserAfter = %v, want fallback to top-level %v", effective.AwaitingUserAfter, cfg.Thresholds.AwaitingUserAfter)
	}
}

func TestLoadConfigInvalidJevMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threadwatch.yaml")
	if err := os.WriteFile(path, []byte("jev:\n  mode: chaotic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for invalid jev.mode")
	}
}

func TestLoadConfigInvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threadwatch.yaml")
	if err := os.WriteFile(path, []byte("thresholds:\n  awaiting_user_after: notaduration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for invalid duration string")
	}
}

func TestStoreAppendAndReadEvents(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: day1, Instance: "a", Kind: KindTurnEnd},
		{Time: day2, Instance: "b", Kind: KindAPIError},
	}
	if err := store.AppendEvents(events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	for _, name := range []string{"events-2026-01-01.jsonl", "events-2026-01-02.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}

	got, err := store.ReadEvents(day1.Add(-time.Hour), day2.Add(time.Hour))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ReadEvents returned %d events, want 2: %+v", len(got), got)
	}

	got, err = store.ReadEvents(day1.Add(-time.Hour), day1.Add(time.Hour))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(got) != 1 || got[0].Instance != "a" {
		t.Fatalf("ReadEvents narrow range = %+v, want just day1's event", got)
	}
}

func TestStoreAppendAndReadSignals(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	when := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	signals := []Signal{{Time: when, Instance: "a", Code: CodeAwaitingUser, Tier: TierIntervene}}
	if err := store.AppendSignals(signals); err != nil {
		t.Fatalf("AppendSignals: %v", err)
	}

	got, err := store.ReadSignals(when.Add(-time.Minute), when.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadSignals: %v", err)
	}
	if len(got) != 1 || got[0].Code != CodeAwaitingUser {
		t.Fatalf("ReadSignals = %+v", got)
	}
}

func TestStoreReadEventsSkipsCorruptTrailingLine(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	day := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := store.AppendEvents([]Event{{Time: day.Add(time.Hour), Instance: "a", Kind: KindTurnEnd}}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	path := filepath.Join(dir, "events-2026-02-01.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"time":"2026-02-01T02:00:00Z","instance":"b","kind":"tur`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReadEvents(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("ReadEvents with corrupt trailing line: %v", err)
	}
	if len(got) != 1 || got[0].Instance != "a" {
		t.Fatalf("ReadEvents = %+v, want just the well-formed event", got)
	}
}

func TestStorePrune(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -20)
	recent := now.AddDate(0, 0, -1)

	if err := store.AppendEvents([]Event{{Time: old, Instance: "a"}, {Time: recent, Instance: "a"}}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if err := store.AppendSignals([]Signal{{Time: old, Instance: "a", Code: CodeStalledTurn}}); err != nil {
		t.Fatalf("AppendSignals: %v", err)
	}

	if err := store.Prune(now, 14*24*time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	oldEvents := filepath.Join(dir, "events-"+old.Format("2006-01-02")+".jsonl")
	if _, err := os.Stat(oldEvents); !os.IsNotExist(err) {
		t.Errorf("expected old events file to be pruned, stat err = %v", err)
	}
	oldSignals := filepath.Join(dir, "signals-"+old.Format("2006-01-02")+".jsonl")
	if _, err := os.Stat(oldSignals); !os.IsNotExist(err) {
		t.Errorf("expected old signals file to be pruned, stat err = %v", err)
	}
	recentEvents := filepath.Join(dir, "events-"+recent.Format("2006-01-02")+".jsonl")
	if _, err := os.Stat(recentEvents); err != nil {
		t.Errorf("expected recent events file to survive: %v", err)
	}
}

func TestStorePruneDefaultKeep(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	tooOld := now.AddDate(0, 0, -15)
	if err := store.AppendEvents([]Event{{Time: tooOld, Instance: "a"}}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	if err := store.Prune(now, 0); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	path := filepath.Join(dir, "events-"+tooOld.Format("2006-01-02")+".jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file older than default 14d keep to be pruned, stat err = %v", err)
	}
}

func TestStoreConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	const n = 50
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			done <- store.AppendEvents([]Event{{Time: when, Instance: "a", Kind: KindActivity}})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Errorf("AppendEvents goroutine: %v", err)
		}
	}

	got, err := store.ReadEvents(when, when.Add(time.Hour))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(got) != n {
		t.Fatalf("ReadEvents returned %d events, want %d", len(got), n)
	}
}

func TestLoadConfigAPIKeySettings(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		mode        os.FileMode
		wantKey     string
		wantRef     string
		wantProblem string
	}{
		{"none", "jev:\n  mode: shadow\n", 0o600, "", "", ""},
		{"literal in private file", "jev:\n  api_key: ts_abc\n", 0o600, "ts_abc", "", ""},
		{"literal in readable file", "jev:\n  api_key: ts_abc\n", 0o644, "", "", "chmod 600"},
		{"op reference", "jev:\n  api_key_ref: op://vault/item/credential\n", 0o644, "", "op://vault/item/credential", ""},
		{"op reference with section", "jev:\n  api_key_ref: op://vault/item/section/field\n", 0o644, "", "op://vault/item/section/field", ""},
		{"malformed reference", "jev:\n  api_key_ref: vault/item\n", 0o600, "", "", "op://"},
		{"both", "jev:\n  api_key: ts_abc\n  api_key_ref: op://v/i/f\n", 0o600, "", "", "not both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "threadwatch.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Jev.APIKey != tc.wantKey || cfg.Jev.APIKeyRef != tc.wantRef {
				t.Errorf("key=%q ref=%q, want %q %q", cfg.Jev.APIKey, cfg.Jev.APIKeyRef, tc.wantKey, tc.wantRef)
			}
			if tc.wantProblem == "" && cfg.Jev.KeyProblem != "" || !strings.Contains(cfg.Jev.KeyProblem, tc.wantProblem) {
				t.Errorf("problem = %q, want containing %q", cfg.Jev.KeyProblem, tc.wantProblem)
			}
			if strings.Contains(cfg.Jev.KeyProblem, "ts_abc") {
				t.Error("problem text leaks the key")
			}
		})
	}
}
