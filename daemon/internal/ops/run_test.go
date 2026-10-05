package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// runEnv is an amp instance registry entry plus a fake HOME for state.
type runEnv struct {
	home string
}

func newRunEnv(t *testing.T, extra ...string) *runEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	workdir := filepath.Join(root, "work")
	for _, d := range []string{home, workdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	lines := []string{"AGENTMUX_AGENT=amp", "AGENTMUX_WORKDIR=" + workdir, "AGENTMUX_AMP_RUNNER_ID=probe"}
	lines = append(lines, extra...)
	if err := os.WriteFile(filepath.Join(envDir, "probe.env"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &runEnv{home: home}
}

func localAddr(thread string) string {
	a := "probe@" + address.LocalHostName()
	if thread != "" {
		a += "#" + thread
	}
	return a
}

func TestRunStartsThread(t *testing.T) {
	c := newRunEnv(t)
	id := "T-11111111-1111-4111-8111-111111111111"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	res, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(""), Text: "do the thing"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if !res.OK || res.ThreadID != id || res.State != "running" {
		t.Fatalf("result = %+v", res)
	}
	if res.ThreadURL != "https://ampcode.com/threads/"+id {
		t.Fatalf("url = %q", res.ThreadURL)
	}
	if res.Address != localAddr(id) {
		t.Fatalf("address = %q", res.Address)
	}
	flat := strings.Join(fake.Argv, " ")
	if !strings.HasPrefix(flat, "--stream-json") || !strings.HasSuffix(flat, "-x do the thing") {
		t.Fatalf("argv = %q", flat)
	}
	if strings.Contains(flat, "--title") {
		t.Fatalf("untitled run names a thread: %q", flat)
	}
	if fake.RenameSeen {
		t.Fatalf("untitled run renamed: %q", fake.Renamed)
	}
	// The log moved under the thread's own name.
	if _, err := os.Stat(session.AmpRunLogPath(c.home, "probe", id)); err != nil {
		t.Fatalf("thread log missing: %v", err)
	}
}

func TestRunPassesConfiguredMode(t *testing.T) {
	newRunEnv(t, "AGENTMUX_AMP_MODE=high")
	id := "T-22222222-2222-4222-8222-222222222222"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	res, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(""), Text: "hi"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	_ = res
	if fake.Mode != "high" {
		t.Fatalf("mode = %q", fake.Mode)
	}
	if !strings.Contains(strings.Join(fake.Argv, " "), "-m high") {
		t.Fatalf("no -m high in %q", fake.Argv)
	}
	if m := AmpModeOf("probe"); m.Mode != "high" || m.Source != "instance" {
		t.Fatalf("AmpModeOf = %+v", m)
	}
}

func TestRunContinuesThread(t *testing.T) {
	newRunEnv(t)
	id := "T-33333333-3333-4333-8333-333333333333"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	res, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(id), Text: "again"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	_ = res
	if flat := strings.Join(fake.Argv, " "); !strings.HasPrefix(flat, "threads continue "+id+" --stream-json") || !strings.HasSuffix(flat, "-x again") {
		t.Fatalf("argv = %q", flat)
	}
}

// TestRunTitlesNewThread passes --title plus --no-archive-after-execute
// for a new thread and re-applies the title once it exists; a continue
// ignores the title entirely.
func TestRunTitlesNewThread(t *testing.T) {
	newRunEnv(t)
	id := "T-77777777-7777-4777-8777-777777777777"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(""), Text: "hi", Title: "AMUX-17 do the thing"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	flat := strings.Join(fake.Argv, " ")
	if !strings.Contains(flat, "--title AMUX-17 do the thing --no-archive-after-execute") || !strings.HasSuffix(flat, "-x hi") {
		t.Fatalf("argv = %q", flat)
	}
	if !fake.RenameSeen || fake.Renamed != "AMUX-17 do the thing" {
		t.Fatalf("rename = %v %q", fake.RenameSeen, fake.Renamed)
	}
}

// TestRunContinueIgnoresTitle continues without --title and without a
// rename: the thread already has its name.
func TestRunContinueIgnoresTitle(t *testing.T) {
	newRunEnv(t)
	id := "T-88888888-8888-4888-8888-888888888888"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(id), Text: "again", Title: "AMUX-17 do the thing"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if flat := strings.Join(fake.Argv, " "); strings.Contains(flat, "--title") {
		t.Fatalf("continue names a thread: %q", flat)
	}
	if fake.RenameSeen {
		t.Fatalf("continue renamed: %q", fake.Renamed)
	}
}

// TestRunRejectsBadTitle refuses an overlong title before spawning.
func TestRunRejectsBadTitle(t *testing.T) {
	newRunEnv(t)
	restore, fake := session.AmpSwapForTest("T-99999999-9999-4999-8999-999999999999", nil)
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(""), Text: "hi", Title: strings.Repeat("x", 300)})
	wantReason(t, rerr, safesend.ReasonInvalid)
	if fake.Argv != nil {
		t.Fatalf("refused run spawned: %q", fake.Argv)
	}
}

func TestRunRefusals(t *testing.T) {
	newRunEnv(t)
	for _, tc := range []struct {
		name string
		addr string
		text string
		want safesend.Reason
	}{
		{"empty text", localAddr(""), "", safesend.ReasonInvalid},
		{"bad thread id", localAddr("bogus"), "hi", safesend.ReasonInvalid},
		{"unknown instance", "ghost@" + address.LocalHostName(), "hi", safesend.ReasonNotFound},
		{"other host", "probe@elsewhere", "hi", safesend.ReasonNotLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore, _ := session.AmpSwapForTest("T-44444444-4444-4444-8444-444444444444", nil)
			defer restore()
			_, rerr := Env{}.Run(context.Background(), RunRequest{Address: tc.addr, Text: tc.text})
			wantReason(t, rerr, tc.want)
		})
	}
	// A claude-code instance is unsupported, not invalid.
	workdir := filepath.Join(t.TempDir(), "w")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discovery.EnvDir, "cc.env"), []byte("AGENTMUX_AGENT=claude-code\nAGENTMUX_WORKDIR="+workdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore, _ := session.AmpSwapForTest("T-55555555-5555-4555-8555-555555555555", nil)
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: "cc@" + address.LocalHostName(), Text: "hi"})
	wantReason(t, rerr, safesend.ReasonUnsupported)
}

func TestRunModeRejection(t *testing.T) {
	newRunEnv(t, "AGENTMUX_AMP_MODE=bogus-mode")
	restore, _ := session.AmpSwapForTest("T-66666666-6666-4666-8666-666666666666",
		errors.New(`amp rejected mode "bogus-mode": Unexpected error inside Amp CLI.`))
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(""), Text: "hi"})
	wantReason(t, rerr, safesend.ReasonInvalid)
	if !strings.Contains(AsError(rerr).Detail, "Unexpected error") {
		t.Fatalf("detail quotes amp: %v", rerr)
	}
}

// TestRunContinueUnarchivesAndKeepsUnarchived continues a thread with an
// unarchive before the spawn (amp archives a thread when an -x run ends,
// and continuing an archived one refuses), and the continued argv keeps
// --no-archive-after-execute so the thread stays unarchived. A fresh start
// unarchives nothing.
func TestRunContinueUnarchivesAndKeepsUnarchived(t *testing.T) {
	newRunEnv(t)
	id := "T-aaaaaaaaaaaaaaaa-4aaa-8aaa-aaaaaaaaaaaa"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(id), Text: "again"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if !fake.UnarchiveSeen || fake.Unarchived != id {
		t.Fatalf("unarchive = %v %q", fake.UnarchiveSeen, fake.Unarchived)
	}
	if fake.UnarchivedAt != 0 {
		t.Fatalf("unarchive ran after spawn: %d", fake.UnarchivedAt)
	}
	if flat := strings.Join(fake.Argv, " "); !strings.Contains(flat, "--no-archive-after-execute") {
		t.Fatalf("continued run archives: %q", flat)
	}
}

// TestRunContinueWaitingStopsFirst covers answering a pending
// ask_user_choice question: the continue stops the stuck process first,
// then unarchives, then spawns — stop, unarchive, spawn in that order.
// The fake log carries a pending question (no result record after it).
func TestRunContinueWaitingStopsFirst(t *testing.T) {
	newRunEnv(t)
	id := "T-bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	// Write the waiting log under the thread's own name before continuing.
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".local", "state", "agentmux", "sessions", "probe")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"` + id + `"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"TU-1","name":"ask_user_choice","input":{"question":"Tabs or spaces?","options":["Tabs","Spaces"]}}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "amp-run-"+id+".jsonl"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(id), Text: "Spaces"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if !fake.StopSeen || fake.Stopped != id {
		t.Fatalf("stop = %v %q", fake.StopSeen, fake.Stopped)
	}
	if fake.StopWorkdir == "" {
		t.Fatal("stop ran without the instance workdir scope")
	}
	if !fake.UnarchiveSeen || fake.Unarchived != id {
		t.Fatalf("unarchive = %v %q", fake.UnarchiveSeen, fake.Unarchived)
	}
	if !(fake.StoppedAt == 0 && fake.UnarchivedAt == 0) {
		t.Fatalf("stop/unarchive ran after spawn: stopped=%d unarchived=%d", fake.StoppedAt, fake.UnarchivedAt)
	}
}

// TestRunContinueRunningStopsNothing covers the normal continue: no
// pending question in the log means no stop — only the unarchive runs
// before the spawn.
func TestRunContinueRunningStopsNothing(t *testing.T) {
	newRunEnv(t)
	id := "T-cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	_, rerr := Env{}.Run(context.Background(), RunRequest{Address: localAddr(id), Text: "again"})
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if fake.StopSeen {
		t.Fatalf("stop ran with no pending question: %q", fake.Stopped)
	}
	if !fake.UnarchiveSeen || fake.Unarchived != id {
		t.Fatalf("unarchive = %v %q", fake.UnarchiveSeen, fake.Unarchived)
	}
}
