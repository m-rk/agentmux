package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fakeOpToken = "fake-service-account-token"

// opHome points HOME at a temp dir and, when withToken, provisions a
// service account token file there.
func opHome(t *testing.T, withToken bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if withToken {
		p := filepath.Join(home, opTokenRelPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(fakeOpToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func writeOpEnvFile(t *testing.T, home, name string) string {
	t.Helper()
	p := filepath.Join(home, ".agentmux", "env", name+".env")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("AMP_API_KEY=op://<vault>/<item>/<field>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeAmpRegistry(t *testing.T, dir, workdir string) {
	t.Helper()
	body := "" +
		"AGENTMUX_INSTANCE_NAME=probe\n" +
		"AGENTMUX_AGENT=amp\n" +
		"AGENTMUX_AMP_RUNNER_ID=probe\n" +
		"AGENTMUX_TMUX_SESSION_NAME=probe\n" +
		"AGENTMUX_WORKDIR=" + workdir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.env"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeTmux records tmux calls and reports the session as not yet running.
func fakeTmux(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	prev := withPath
	withPath = func(name string, args ...string) *exec.Cmd {
		if name != "tmux" {
			t.Fatalf("withPath called with unexpected command %q", name)
		}
		calls = append(calls, args)
		if slices.Contains(args, "has-session") {
			return exec.Command("false")
		}
		return exec.Command("true")
	}
	t.Cleanup(func() { withPath = prev })
	return &calls
}

func newSessionArgs(t *testing.T, calls [][]string) []string {
	t.Helper()
	for _, args := range calls {
		if slices.Contains(args, "new-session") {
			return args
		}
	}
	t.Fatalf("no tmux new-session issued; calls: %v", calls)
	return nil
}

func TestOpRunArgsStripsTheTokenFromTheAgent(t *testing.T) {
	got := opRunArgs("/e/probe.env", []string{"amp", "--no-tui"})
	want := []string{"run", "--env-file=/e/probe.env", "--", "/usr/bin/env", "-u", "OP_SERVICE_ACCOUNT_TOKEN", "amp", "--no-tui"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("opRunArgs = %v, want %v", got, want)
	}
}

// Instances with no env-file must launch byte-identically to before.
func TestRunAmpWithoutEnvFileIsUnchanged(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	opHome(t, true) // a token alone must not change anything
	writeAmpRegistry(t, dir, workdir)
	calls := fakeTmux(t)

	if err := RunAmp("probe"); err != nil {
		t.Fatalf("RunAmp: %v", err)
	}
	want := []string{
		"-L", "agentmux-probe", "new-session", "-d", "-s", "probe", "-c", workdir,
		"amp", "--no-tui", "--runner-id", "probe", "--remote-control-terminal",
	}
	if got := newSessionArgs(t, *calls); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("launched\n  %v\nwant\n  %v", got, want)
	}
}

func TestRunAmpWithEnvFileReentersAgentmuxWithoutSecrets(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	home := opHome(t, true)
	writeOpEnvFile(t, home, "probe")
	writeAmpRegistry(t, dir, workdir)
	calls := fakeTmux(t)

	// op must be resolvable for the preflight.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "op"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	if err := RunAmp("probe"); err != nil {
		t.Fatalf("RunAmp: %v", err)
	}
	got := newSessionArgs(t, *calls)
	n := len(got)
	if got[n-5] == "" || got[n-4] != "session" || got[n-3] != "exec" || got[n-2] != "--instance" || got[n-1] != "probe" {
		t.Errorf("expected `<agentmux> session exec --instance probe`, got %v", got[n-5:])
	}
	if strings.Contains(strings.Join(got, " "), fakeOpToken) {
		t.Errorf("token leaked into tmux argv: %v", got)
	}
}

func TestRunAmpWithEnvFileFailsVisiblyWithoutToken(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	home := opHome(t, false)
	writeOpEnvFile(t, home, "probe")
	writeAmpRegistry(t, dir, workdir)
	calls := fakeTmux(t)

	err := RunAmp("probe")
	if err == nil {
		t.Fatal("RunAmp succeeded with an env-file but no service account token")
	}
	if !strings.Contains(err.Error(), "service account token") {
		t.Errorf("error should name the missing token, got: %v", err)
	}
	for _, args := range *calls {
		if slices.Contains(args, "new-session") {
			t.Errorf("a session was started despite the failed preflight: %v", args)
		}
	}
}

func TestExecAmpRunsOpWithTokenInEnvOnly(t *testing.T) {
	dir := withEnvDir(t)
	workdir := t.TempDir()
	home := opHome(t, true)
	envFile := writeOpEnvFile(t, home, "probe")
	writeAmpRegistry(t, dir, workdir)

	var gotPath string
	var gotArgv, gotEnv []string
	prev := execSyscall
	execSyscall = func(path string, argv, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	}
	t.Cleanup(func() { execSyscall = prev })

	if err := ExecAmp("probe"); err != nil {
		t.Fatalf("ExecAmp: %v", err)
	}
	if filepath.Base(gotPath) != "op" {
		t.Errorf("exec'd %q, want op", gotPath)
	}
	wantTail := []string{
		"run", "--env-file=" + envFile, "--", "/usr/bin/env", "-u", "OP_SERVICE_ACCOUNT_TOKEN",
		"amp", "--no-tui", "--runner-id", "probe", "--remote-control-terminal",
	}
	if len(gotArgv) < 1 || strings.Join(gotArgv[1:], "\x00") != strings.Join(wantTail, "\x00") {
		t.Errorf("argv = %v, want op + %v", gotArgv, wantTail)
	}
	if strings.Contains(strings.Join(gotArgv, " "), fakeOpToken) {
		t.Errorf("token leaked into argv: %v", gotArgv)
	}
	if !slices.Contains(gotEnv, "OP_SERVICE_ACCOUNT_TOKEN="+fakeOpToken) {
		t.Errorf("token missing from the exec environment")
	}
}

func TestExecAmpRefusesWithoutEnvFile(t *testing.T) {
	dir := withEnvDir(t)
	opHome(t, true)
	writeAmpRegistry(t, dir, t.TempDir())
	prev := execSyscall
	execSyscall = func(string, []string, []string) error { t.Fatal("exec called"); return nil }
	t.Cleanup(func() { execSyscall = prev })
	if err := ExecAmp("probe"); err == nil {
		t.Fatal("ExecAmp succeeded with no env-file")
	}
}

func TestExecWithOpEnvPassesTokenAndMarkerOnlyInEnv(t *testing.T) {
	opHome(t, true)
	prevPath := withPath
	withPath = func(name string, args ...string) *exec.Cmd { return exec.Command("/usr/bin/"+name, args...) }
	t.Cleanup(func() { withPath = prevPath })
	var gotArgv, gotEnv []string
	prev := execSyscall
	execSyscall = func(path string, argv, env []string) error {
		gotArgv, gotEnv = argv, env
		return nil
	}
	t.Cleanup(func() { execSyscall = prev })

	if err := ExecWithOpEnv("/e/tw.env", "MARKER", []string{"/bin/agentmux", "threadwatch", "serve"}); err != nil {
		t.Fatalf("ExecWithOpEnv: %v", err)
	}
	if strings.Contains(strings.Join(gotArgv, " "), fakeOpToken) {
		t.Fatal("token leaked into argv")
	}
	want := "op run --env-file=/e/tw.env -- /usr/bin/env -u OP_SERVICE_ACCOUNT_TOKEN /bin/agentmux threadwatch serve"
	if got := strings.Join(append([]string{"op"}, gotArgv[1:]...), " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if !slices.Contains(gotEnv, "OP_SERVICE_ACCOUNT_TOKEN="+fakeOpToken) || !slices.Contains(gotEnv, "MARKER=1") {
		t.Error("token or marker missing from env")
	}
}

func TestExecWithOpEnvRefusesWithoutToken(t *testing.T) {
	opHome(t, false)
	prev := execSyscall
	execSyscall = func(string, []string, []string) error { t.Fatal("exec called"); return nil }
	t.Cleanup(func() { execSyscall = prev })
	if err := ExecWithOpEnv("/e/tw.env", "MARKER", []string{"x"}); err == nil {
		t.Fatal("expected an error without a token file")
	}
}
