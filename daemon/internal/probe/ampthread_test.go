package probe

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAmp plays amp for AmpStanding.try: each new thread gets the next
// id, and gone lists the threads amp refuses to continue.
type fakeAmp struct {
	calls [][]string
	next  int
	gone  map[string]bool
}

func (f *fakeAmp) run(args []string) Attempt {
	f.calls = append(f.calls, args)
	if args[0] == "threads" {
		id := args[2]
		if f.gone[id] {
			return Attempt{Err: &Failure{Words: "exited"}, threadGone: true}
		}
		return Attempt{Session: id}
	}
	f.next++
	return Attempt{Session: "T-" + string(rune('a'+f.next-1))}
}

func standing(t *testing.T, now *time.Time) (*AmpStanding, *[]string) {
	var log []string
	return &AmpStanding{
		Path: filepath.Join(t.TempDir(), "amp-probe-thread.json"),
		Now:  func() time.Time { return *now },
		Log:  func(l string) { log = append(log, l) },
	}, &log
}

func TestAmpStandingReusesOneThread(t *testing.T) {
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	s, log := standing(t, &now)
	f := &fakeAmp{}
	tg := Target{Provider: "amp", Runner: "amp", Model: "rush"}
	for i := 0; i < 3; i++ {
		if a := s.try(tg, f.run); a.Session != "T-a" {
			t.Fatalf("run %d used %q", i, a.Session)
		}
		now = now.Add(time.Hour)
	}
	if f.next != 1 {
		t.Fatalf("started %d threads, want 1", f.next)
	}
	first := strings.Join(f.calls[0], " ")
	if !strings.Contains(first, "--no-archive-after-execute") || !strings.HasSuffix(first, "-x "+AmpThreadPrefix+" "+Prompt) {
		t.Errorf("new thread args = %q", first)
	}
	if got := f.calls[1]; got[0] != "threads" || got[1] != "continue" || got[2] != "T-a" || got[len(got)-1] != Prompt {
		t.Errorf("continue args = %q", got)
	}
	st, _ := LoadAmpThread(s.Path)
	if st.ID != "T-a" || st.Turns != 3 {
		t.Errorf("state = %+v", st)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "T-a: continued (turn 3)") {
		t.Errorf("log = %q", *log)
	}
}

func TestAmpStandingReplacesGoneThread(t *testing.T) {
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	s, log := standing(t, &now)
	if err := SaveAmpThread(s.Path, AmpThread{ID: "T-old", StartedAt: now, Turns: 4}); err != nil {
		t.Fatal(err)
	}
	f := &fakeAmp{gone: map[string]bool{"T-old": true}}
	a := s.try(Target{Runner: "amp", Model: "rush"}, f.run)
	if a.Err != nil || a.Session != "T-a" || len(f.calls) != 2 {
		t.Fatalf("attempt %+v after %d calls", a, len(f.calls))
	}
	if st, _ := LoadAmpThread(s.Path); st.ID != "T-a" || st.Turns != 1 {
		t.Errorf("state = %+v", st)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "T-old: gone or archived") {
		t.Errorf("log = %q", *log)
	}
}

func TestAmpStandingRollsRarely(t *testing.T) {
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	s, _ := standing(t, &now)
	f := &fakeAmp{}
	_ = SaveAmpThread(s.Path, AmpThread{ID: "T-young", StartedAt: now.Add(-29 * 24 * time.Hour), Turns: AmpRollTurns - 1})
	if a := s.try(Target{Runner: "amp"}, f.run); a.Session != "T-young" {
		t.Fatalf("young thread rolled: %+v", a)
	}
	if a := s.try(Target{Runner: "amp"}, f.run); a.Session != "T-a" {
		t.Fatalf("thread past the turn cap kept: %+v", a)
	}
	_ = SaveAmpThread(s.Path, AmpThread{ID: "T-old", StartedAt: now.Add(-AmpRollAge), Turns: 2})
	if a := s.try(Target{Runner: "amp"}, f.run); a.Session != "T-b" {
		t.Fatalf("month-old thread kept: %+v", a)
	}
}

func TestAmpStandingFailedContinueKeepsThread(t *testing.T) {
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	s, _ := standing(t, &now)
	_ = SaveAmpThread(s.Path, AmpThread{ID: "T-x", StartedAt: now, Turns: 1})
	calls := 0
	a := s.try(Target{Runner: "amp"}, func([]string) Attempt {
		calls++
		return Attempt{Err: &Failure{Words: "usage limit"}}
	})
	if a.Err == nil || calls != 1 {
		t.Fatalf("attempt %+v after %d calls", a, calls)
	}
	if st, _ := LoadAmpThread(s.Path); st.ID != "T-x" {
		t.Errorf("a quota failure dropped the thread: %+v", st)
	}
}

func TestThreadGone(t *testing.T) {
	for _, s := range []string{
		"Thread https://ampcode.com/threads/T-0 does not exist.",
		"This thread is archived and cannot be continued",
	} {
		if !threadGone(s) {
			t.Errorf("threadGone(%q) = false", s)
		}
	}
	if threadGone("usage limit reached") {
		t.Error("quota counted as gone")
	}
}
