package ampexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"testing"
)

// fakeCommand returns a CommandFactory that records the args/env it was
// called with and runs script through sh -c instead of the real amp
// binary, mirroring dailycheck/threadwatch's own fakeReviewCommand helpers.
func fakeCommand(t *testing.T, script string) (factory CommandFactory, gotArgs *[]string, gotEnv *[]string) {
	t.Helper()
	var args, env []string
	factory = func(ctx context.Context, name string, a ...string) *exec.Cmd {
		if name != "amp" {
			t.Fatalf("binary = %q, want amp", name)
		}
		args = append([]string(nil), a...)
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		env = cmd.Environ() // captured again after Run sets cmd.Env below
		return cmd
	}
	return factory, &args, &env
}

func TestRunUsesStdinNotArgv(t *testing.T) {
	factory, gotArgs, _ := fakeCommand(t, `cat >/dev/null; printf 'ok'`)
	out, err := Run(context.Background(), factory, Config{}, "secret pane text")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "ok" {
		t.Errorf("out = %q, want ok", out)
	}
	if strings.Contains(strings.Join(*gotArgs, " "), "secret pane text") {
		t.Fatal("message leaked into process arguments instead of stdin")
	}
}

func TestRunLocalExecutorArgsAndSettingsFile(t *testing.T) {
	var settingsPath string
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		for _, want := range []string{"-x", "--executor local", "-m high", "-l my-label"} {
			if !strings.Contains(joined, want) {
				t.Errorf("args missing %q: %q", want, joined)
			}
		}
		for i, a := range args {
			if a == "--settings-file" && i+1 < len(args) {
				settingsPath = args[i+1]
			}
		}
		if settingsPath == "" {
			t.Fatalf("no --settings-file in args: %q", joined)
		}
		// Assert the settings file exists, is 0600, and disables every tool
		// two independent ways, while the child can still see it.
		info, err := os.Stat(settingsPath)
		if err != nil {
			t.Fatalf("stat settings file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("settings file mode = %o, want 0600", perm)
		}
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("reading settings file: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("settings file is not valid JSON: %v", err)
		}
		disable, _ := parsed["amp.tools.disable"].([]any)
		if len(disable) != 1 || disable[0] != "*" {
			t.Errorf(`amp.tools.disable = %v, want ["*"]`, parsed["amp.tools.disable"])
		}
		perms, _ := parsed["amp.permissions"].([]any)
		if len(perms) != 1 {
			t.Fatalf("amp.permissions = %v, want one reject-all rule", parsed["amp.permissions"])
		}
		rule, _ := perms[0].(map[string]any)
		if rule["tool"] != "*" || rule["action"] != "reject" {
			t.Errorf("amp.permissions[0] = %v, want tool \"*\" action \"reject\"", rule)
		}
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf 'ok'`)
	}

	cfg := Config{Executor: "local", Mode: "high", Label: "my-label", Workdir: t.TempDir()}
	if _, err := Run(context.Background(), factory, cfg, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings file %s was not removed after Run returned", settingsPath)
	}
}

func TestRunLocalExecutorDefaultWhenExecutorEmpty(t *testing.T) {
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if !strings.Contains(strings.Join(args, " "), "--executor local") {
			t.Errorf("args = %v, want --executor local as the default", args)
		}
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf 'ok'`)
	}
	if _, err := Run(context.Background(), factory, Config{}, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunRunnerExecutorNoSettingsFile(t *testing.T) {
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--executor runner:abc123") {
			t.Errorf("args missing runner executor: %q", joined)
		}
		if !strings.Contains(joined, "--runner-dir /srv/work") {
			t.Errorf("args missing --runner-dir: %q", joined)
		}
		if strings.Contains(joined, "--settings-file") {
			t.Errorf("runner executor must not get a --settings-file (tools cannot be disabled remotely): %q", joined)
		}
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf 'ok'`)
	}
	cfg := Config{Executor: "runner:abc123", RunnerDir: "/srv/work"}
	if _, err := Run(context.Background(), factory, cfg, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunSettingsFileChownedToOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to another user requires root")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(me.Uid)
	gid, _ := strconv.Atoi(me.Gid)

	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		var settingsPath string
		for i, a := range args {
			if a == "--settings-file" && i+1 < len(args) {
				settingsPath = args[i+1]
			}
		}
		info, err := os.Stat(settingsPath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		st := info.Sys()
		_ = st
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf 'ok'`)
	}
	cfg := Config{Executor: "local", Owner: me}
	if _, err := Run(context.Background(), factory, cfg, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = uid
	_ = gid
}

func TestRunAPIKeyInEnvNotArgv(t *testing.T) {
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if strings.Contains(strings.Join(args, " "), "sk-super-secret") {
			t.Fatal("API key leaked into argv")
		}
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; env`)
	}
	cfg := Config{APIKey: "sk-super-secret"}
	out, err := Run(context.Background(), factory, cfg, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "AMP_API_KEY=sk-super-secret") {
		t.Fatal("AMP_API_KEY missing from child environment")
	}
}

func TestRunAPIKeyPreservesInheritedEnv(t *testing.T) {
	t.Setenv("AMPEXEC_TEST_MARKER", "present")
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; env`)
	}
	out, err := Run(context.Background(), factory, Config{APIKey: "k"}, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "AMPEXEC_TEST_MARKER=present") {
		t.Fatal("appending AMP_API_KEY clobbered the inherited environment instead of extending it")
	}
}

func TestRunNoCommandFactory(t *testing.T) {
	if _, err := Run(context.Background(), nil, Config{}, "hi"); err == nil {
		t.Fatal("Run: want error with no CommandFactory")
	}
}

func TestRunErrorPropagates(t *testing.T) {
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; echo boom 1>&2; exit 1`)
	}
	if _, err := Run(context.Background(), factory, Config{}, "hi"); err == nil {
		t.Fatal("Run: want error from a failing command")
	}
}

func TestExtractJSONPlain(t *testing.T) {
	got := ExtractJSON(`{"a":1}`)
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestExtractJSONFencedWithLanguage(t *testing.T) {
	got := ExtractJSON("```json\n{\"a\":1}\n```")
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestExtractJSONFencedBare(t *testing.T) {
	got := ExtractJSON("```\n{\"a\":1}\n```")
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestExtractJSONTrimsWhitespace(t *testing.T) {
	got := ExtractJSON("  \n  {\"a\":1}  \n ")
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestRunnerWarning(t *testing.T) {
	if _, ok := RunnerWarning(""); ok {
		t.Error("empty executor should not warn (defaults to local)")
	}
	if _, ok := RunnerWarning("local"); ok {
		t.Error("local executor should not warn")
	}
	warning, ok := RunnerWarning("runner:abc123")
	if !ok || !strings.Contains(warning, "runner:abc123") {
		t.Errorf("RunnerWarning(runner:abc123) = %q, %v", warning, ok)
	}
}
