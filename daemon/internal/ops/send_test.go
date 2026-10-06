package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// sendEnv is an amp instance registry entry plus a fake HOME for the
// send audit log and the run-log state dir.
type sendEnv struct {
	home string
}

func newSendEnv(t *testing.T, extra ...string) *sendEnv {
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
	return &sendEnv{home: home}
}

func sendReq(addr, text string) SendRequest {
	return SendRequest{Address: addr, Text: text, Via: "relayed", By: "orchestrator"}
}

// TestSendAmpResumesNewestRecordedThread is the AMUX-37 guard: a send to an
// amp instance without a thread suffix resumes the worker's current thread
// through the run path (host/instance mode included), never pasting into
// the runner's terminal. The result names the resumed thread and confirms
// only that its run is running.
func TestSendAmpResumesNewestRecordedThread(t *testing.T) {
	c := newSendEnv(t)
	old := "T-11111111-1111-4111-8111-111111111111"
	new := "T-22222222-2222-4222-8222-222222222222"
	dir := filepath.Join(c.home, ".local", "state", "agentmux", "sessions", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old, new} {
		if err := os.WriteFile(filepath.Join(dir, "amp-run-"+id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct mod times so newest-first ordering is deterministic: the
	// second file must win.
	if err := os.Chtimes(filepath.Join(dir, "amp-run-"+old+".jsonl"), time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	restore, fake := session.AmpSwapForTest(new, nil)
	defer restore()
	res := Env{}.Send(context.Background(), sendReq(localAddr(""), "carry on"))
	if !res.OK {
		t.Fatalf("send: %+v", res)
	}
	if res.Agent != "amp" || res.Thread != new || res.Address != localAddr(new) {
		t.Fatalf("result = %+v, want the resumed thread %s", res, new)
	}
	if !res.Confirmed {
		t.Fatalf("result = %+v, want confirmed: the run reports the worker running", res)
	}
	flat := strings.Join(fake.Argv, " ")
	if !strings.HasPrefix(flat, "threads continue "+new+" --stream-json") || !strings.HasSuffix(flat, "-x [relayed by orchestrator]\ncarry on") {
		t.Fatalf("argv = %q, want a continue of the recorded thread with the provenance prefix", flat)
	}
	for _, bad := range []string{"--execute=", "--executor", "--orb-execute"} {
		if strings.Contains(flat, bad) {
			t.Fatalf("argv = %q: a send must never use the legacy terminal-paste flags", flat)
		}
	}
	// The audit log says resumed, never delivered: no reader should mistake
	// this for a terminal paste.
	audit, err := os.ReadFile(filepath.Join(c.home, ".local", "state", "agentmux", "send-audit.jsonl"))
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	if !strings.Contains(string(audit), `"outcome":"resumed"`) || strings.Contains(string(audit), `"outcome":"delivered"`) {
		t.Fatalf("audit = %s, want a resumed outcome", audit)
	}
}

// TestSendAmpKeepsExplicitThread checks the address suffix wins over the
// recorded newest thread: a relay that already knows the worker's thread
// resumes exactly it.
func TestSendAmpKeepsExplicitThread(t *testing.T) {
	c := newSendEnv(t)
	want := "T-33333333-3333-4333-8333-333333333333"
	other := "T-44444444-4444-4334-8333-444444444444"
	dir := filepath.Join(c.home, ".local", "state", "agentmux", "sessions", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The other log is newer; the explicit suffix must still win.
	if err := os.WriteFile(filepath.Join(dir, "amp-run-"+want+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "amp-run-"+want+".jsonl"), time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "amp-run-"+other+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, fake := session.AmpSwapForTest(want, nil)
	defer restore()
	res := Env{}.Send(context.Background(), sendReq(localAddr(want), "again"))
	if !res.OK || res.Thread != want {
		t.Fatalf("result = %+v, want the explicit thread %s", res, want)
	}
	if flat := strings.Join(fake.Argv, " "); !strings.HasPrefix(flat, "threads continue "+want+" --stream-json") {
		t.Fatalf("argv = %q, want a continue of the explicit thread", flat)
	}
}

// TestSendAmpRefusesWithoutThread is the other half of AMUX-37: with no
// recorded or listed thread there is nothing to resume, so the send is
// refused (not_found) with the sessions run to use instead — never a new
// thread on the default model.
func TestSendAmpRefusesWithoutThread(t *testing.T) {
	newSendEnv(t)
	restore, fake := session.AmpSwapForTest("T-55555555-5555-4555-8555-555555555555", nil)
	defer restore()
	res := Env{}.Send(context.Background(), sendReq(localAddr(""), "hello?"))
	if res.OK || res.Reason != safesend.ReasonNotFound {
		t.Fatalf("result = %+v, want a not_found refusal", res)
	}
	if !strings.Contains(res.Detail, "sessions run") {
		t.Fatalf("detail = %q, want the sessions run to use instead", res.Detail)
	}
	if len(fake.Argv) != 0 || fake.Checked != 0 {
		t.Fatalf("spawn = %q checked = %d: a refused send starts nothing", fake.Argv, fake.Checked)
	}
}

// TestSendAmpRefusesBadThreadId checks a malformed suffix is invalid
// before anything spawns.
func TestSendAmpRefusesBadThreadId(t *testing.T) {
	newSendEnv(t)
	restore, fake := session.AmpSwapForTest("T-66666666-6666-4666-8666-666666666666", nil)
	defer restore()
	res := Env{}.Send(context.Background(), sendReq(localAddr("nope"), "hi"))
	if res.OK || res.Reason != safesend.ReasonInvalid {
		t.Fatalf("result = %+v, want an invalid refusal", res)
	}
	if len(fake.Argv) != 0 {
		t.Fatalf("spawn = %q: a refused send starts nothing", fake.Argv)
	}
}

// TestSendAmpPropagatesRunRefusal checks a resume the run path refuses
// (here: no runner id, so no run path at all) comes back as that refusal,
// never as a delivered send.
func TestSendAmpPropagatesRunRefusal(t *testing.T) {
	c := newSendEnv(t, "AGENTMUX_AMP_RUNNER_ID=")
	_ = c
	id := "T-77777777-7777-4777-8777-777777777777"
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	res := Env{}.Send(context.Background(), sendReq(localAddr(id), "hi"))
	if res.OK || res.Reason != safesend.ReasonUnsupported {
		t.Fatalf("result = %+v, want the run path's refusal", res)
	}
	if len(fake.Argv) != 0 {
		t.Fatalf("spawn = %q: a refused send starts nothing", fake.Argv)
	}
}

// TestSendRejectsDoorbellForAmp: a wake-up nudge cannot resume a thread —
// there is no doorbell to coalesce into — so it is refused as unsupported,
// and starts nothing.
func TestSendRejectsDoorbellForAmp(t *testing.T) {
	c := newSendEnv(t)
	id := "T-88888888-8888-4888-8888-888888888888"
	dir := filepath.Join(c.home, ".local", "state", "agentmux", "sessions", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "amp-run-"+id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, fake := session.AmpSwapForTest(id, nil)
	defer restore()
	req := sendReq(localAddr(""), "wake")
	req.Doorbell = true
	res := Env{}.Send(context.Background(), req)
	if res.OK || res.Reason != safesend.ReasonUnsupported {
		t.Fatalf("result = %+v, want an unsupported refusal", res)
	}
	if len(fake.Argv) != 0 {
		t.Fatalf("spawn = %q: a refused send starts nothing", fake.Argv)
	}
}
