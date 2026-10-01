package paseoupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeHost simulates npm + the paseo CLI + its daemon. brokenVersions never
// come up healthy after a restart; flaky versions come up and then die.
type fakeHost struct {
	installed string
	latest    string
	daemonVer string
	running   bool
	broken    map[string]bool
	flaky     map[string]bool
	failNpm   map[string]bool
	restarts  int
	sleeps    int
	notes     []string
}

func (h *fakeHost) deps() Deps {
	return Deps{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			switch {
			case cmd == "paseo --version":
				return h.installed + "\n", nil
			case strings.HasPrefix(cmd, "npm view"):
				return h.latest + "\n", nil
			case strings.HasPrefix(cmd, "npm install -g "):
				v := strings.TrimPrefix(cmd, "npm install -g "+Package+"@")
				if h.failNpm[v] {
					return "EACCES", errors.New("exit 1")
				}
				h.installed = v
				return "", nil
			case cmd == "paseo daemon status --json":
				state := "stopped"
				if h.running {
					state = "running"
				}
				return fmt.Sprintf(`{"localDaemon":%q,"daemonVersion":%q}`, state, h.daemonVer), nil
			}
			return "", fmt.Errorf("unexpected command %q", cmd)
		},
		Restart: func(context.Context) error {
			h.restarts++
			h.daemonVer = h.installed
			h.running = !h.broken[h.installed]
			return nil
		},
		Notify: func(m string) { h.notes = append(h.notes, m) },
		Sleep: func(time.Duration) {
			h.sleeps++
			// A flaky release survives the first poll then dies during Settle.
			if h.flaky[h.daemonVer] {
				h.running = false
			}
		},
		HealthTimeout: time.Millisecond,
		Settle:        time.Millisecond,
		Poll:          time.Millisecond,
	}
}

func newHost() *fakeHost {
	return &fakeHost{
		installed: "0.8.0", latest: "0.10.2", daemonVer: "0.8.0", running: true,
		broken: map[string]bool{}, flaky: map[string]bool{}, failNpm: map[string]bool{},
	}
}

func TestUpdateSuccess(t *testing.T) {
	h := newHost()
	res, err := Update(context.Background(), h.deps(), false)
	if err != nil || res.Outcome != Updated {
		t.Fatalf("got %+v, %v", res, err)
	}
	if h.installed != "0.10.2" || h.restarts != 1 || len(h.notes) != 1 || !strings.Contains(h.notes[0], "0.8.0 → 0.10.2") {
		t.Fatalf("installed=%s restarts=%d notes=%q", h.installed, h.restarts, h.notes)
	}
}

func TestUpToDateIsSilent(t *testing.T) {
	h := newHost()
	h.latest = "0.8.0"
	res, err := Update(context.Background(), h.deps(), false)
	if err != nil || res.Outcome != UpToDate || h.restarts != 0 || len(h.notes) != 0 {
		t.Fatalf("got %+v err=%v restarts=%d notes=%q", res, err, h.restarts, h.notes)
	}
}

func TestNeverDowngrades(t *testing.T) {
	h := newHost()
	h.installed, h.daemonVer, h.latest = "0.11.0", "0.11.0", "0.10.2"
	res, _ := Update(context.Background(), h.deps(), false)
	if res.Outcome != UpToDate || h.installed != "0.11.0" {
		t.Fatalf("got %+v installed=%s", res, h.installed)
	}
}

func TestCheckOnlyChangesNothing(t *testing.T) {
	h := newHost()
	res, err := Update(context.Background(), h.deps(), true)
	if err != nil || res.Outcome != Available || h.installed != "0.8.0" || h.restarts != 0 || len(h.notes) != 0 {
		t.Fatalf("got %+v err=%v installed=%s restarts=%d", res, err, h.installed, h.restarts)
	}
}

func TestRefusesPrereleaseLatest(t *testing.T) {
	h := newHost()
	h.latest = "0.11.0-beta.2"
	if _, err := Update(context.Background(), h.deps(), false); err == nil || h.installed != "0.8.0" {
		t.Fatalf("expected refusal, err=%v installed=%s", err, h.installed)
	}
}

func TestRollsBackWhenDaemonWontStart(t *testing.T) {
	h := newHost()
	h.broken["0.10.2"] = true
	res, _ := Update(context.Background(), h.deps(), false)
	if res.Outcome != RolledBack || h.installed != "0.8.0" || !h.running || h.restarts != 2 {
		t.Fatalf("got %+v installed=%s running=%v restarts=%d", res, h.installed, h.running, h.restarts)
	}
	if len(h.notes) != 1 || !strings.Contains(h.notes[0], "Rolled back to 0.8.0") {
		t.Fatalf("notes=%q", h.notes)
	}
}

func TestRollsBackWhenDaemonCrashLoops(t *testing.T) {
	h := newHost()
	h.flaky["0.10.2"] = true
	res, _ := Update(context.Background(), h.deps(), false)
	if res.Outcome != RolledBack || !strings.Contains(res.Detail, "did not stay up") {
		t.Fatalf("got %+v", res)
	}
}

func TestRollsBackWhenNpmInstallFails(t *testing.T) {
	h := newHost()
	h.failNpm["0.10.2"] = true
	res, _ := Update(context.Background(), h.deps(), false)
	if res.Outcome != RolledBack || !strings.Contains(res.Detail, "npm install failed") || h.installed != "0.8.0" {
		t.Fatalf("got %+v installed=%s", res, h.installed)
	}
}

func TestRollbackFailureIsLoud(t *testing.T) {
	h := newHost()
	h.broken["0.10.2"], h.broken["0.8.0"] = true, true
	res, _ := Update(context.Background(), h.deps(), false)
	if res.Outcome != RollbackFailed || len(h.notes) != 1 || !strings.Contains(h.notes[0], "manual attention") {
		t.Fatalf("got %+v notes=%q", res, h.notes)
	}
}
