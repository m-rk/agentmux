package taskcheck

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveAllowsOnlyRegisteredCodexTasksAndConfiguredProjects(t *testing.T) {
	fields := map[string]string{
		"AGENTMUX_INSTANCE_NAME": "task-76",
		"AGENTMUX_AGENT":         "codex",
		"AGENTMUX_PROJECT":       "m-rk/agentmux",
		"AGENTMUX_WORKDIR":       "/work/task-76",
		"AGENTMUX_RUN_USER":      "worker",
	}
	target, err := Resolve("task-76", fields)
	if err != nil || target.Workdir != "/work/task-76" || target.Project != "m-rk/agentmux" || target.RunUser != "worker" {
		t.Fatalf("Resolve = %+v, %v", target, err)
	}

	for _, tc := range []struct {
		name   string
		change func(map[string]string)
	}{
		{"non-task", func(f map[string]string) {}},
		{"non-codex", func(f map[string]string) { f["AGENTMUX_AGENT"] = "claude-code" }},
		{"unapproved-project", func(f map[string]string) { f["AGENTMUX_PROJECT"] = "someone/other" }},
		{"missing-user", func(f map[string]string) { f["AGENTMUX_RUN_USER"] = "" }},
		{"relative-workdir", func(f map[string]string) { f["AGENTMUX_WORKDIR"] = "relative" }},
		{"mismatched-instance", func(f map[string]string) { f["AGENTMUX_INSTANCE_NAME"] = "task-other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := map[string]string{}
			for k, v := range fields {
				copy[k] = v
			}
			tc.change(copy)
			instance := "task-76"
			if tc.name == "non-task" {
				instance = "worker"
			}
			if _, err := Resolve(instance, copy); err == nil {
				t.Fatal("Resolve accepted an invalid task")
			}
		})
	}

	fields["AGENTMUX_PROJECT"] = "m-rk/mergentic"
	if target, err := Resolve("task-76", fields); err != nil || target.Project != "m-rk/mergentic" {
		t.Fatalf("Resolve mergentic = %+v, %v", target, err)
	}
}

func TestClassifySeparatesSandboxDenialsFromTestFailures(t *testing.T) {
	for _, tc := range []struct{ output, want string }{
		{"listen tcp [::1]:0: socket: operation not permitted", "sandbox_denial"},
		{"--- FAIL: TestWantedValue\n    check_test.go:10: got 1, want 2", "test_failure"},
		{"listen tcp [::1]:0: socket: operation not permitted\ncheck_test.go:10: got 1, want 2", "mixed_failure"},
		{"FAIL\texample/pkg", "test_failure"},
	} {
		if got := Classify(tc.output); got != tc.want {
			t.Errorf("Classify(%q) = %q, want %q", tc.output, got, tc.want)
		}
	}
}

func TestSafeEnvDropsInheritedSecretsAndGoOverrides(t *testing.T) {
	env := safeEnv([]string{"HOME=/home/worker", "PATH=/usr/bin", "OPENAI_API_KEY=secret", "GOFLAGS=-exec=evil", "GOENV=/tmp/env"}, map[string]string{
		"GOFLAGS": "",
		"GOENV":   "off",
	})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "evil") {
		t.Fatalf("unsafe inherited env retained: %q", env)
	}
	if !strings.Contains(joined, "GOFLAGS=") || !strings.Contains(joined, "GOENV=off") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("required env missing: %q", env)
	}
}

func TestRunUsesFixedCommandPrivateDirsAndReturnsExit(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if u.Uid == "0" {
		t.Skip("requires an ordinary non-root test user")
	}
	workdir := t.TempDir()
	projectDir := filepath.Join(workdir, "daemon")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module example.test/check\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Instance: "task-9", Agent: "codex", Project: "m-rk/agentmux", Workdir: root, RunUser: currentUsername(t)}

	old := taskTestCommand
	t.Cleanup(func() { taskTestCommand = old })
	var gotUser string
	var gotArgs [][]string
	var invoked *exec.Cmd
	taskTestCommand = func(ctx context.Context, user string, args ...string) *exec.Cmd {
		gotUser = user
		gotArgs = append(gotArgs, append([]string(nil), args...))
		invoked = exec.CommandContext(ctx, "sh", "-c", "printf 'stdout check\\n'; printf 'stderr detail\\n' >&2; exit 7")
		return invoked
	}
	result := Run(context.Background(), target)
	if gotUser != target.RunUser || !reflect.DeepEqual(gotArgs, [][]string{{"test", "./..."}}) {
		t.Fatalf("command user/argv = %q %q", gotUser, gotArgs)
	}
	if result.ExitCode != 7 || result.Category != "test_failure" || !strings.Contains(result.Output, "stdout check") || !strings.Contains(result.Output, "stderr detail") {
		t.Fatalf("Run result = %+v", result)
	}
	checkRoot := filepath.Join(root, "daemon")
	if invoked.Dir != checkRoot || !hasEnvPrefix(invoked.Env, "GOCACHE="+checkRoot+string(os.PathSeparator)+".agentmux-test-") || !hasEnvPrefix(invoked.Env, "TMPDIR="+checkRoot+string(os.PathSeparator)+".agentmux-test-") {
		t.Fatalf("command dir/cache/temp are not scoped to worktree: dir=%q env=%q", invoked.Dir, invoked.Env)
	}
	if hasEnvPrefix(invoked.Env, "OPENAI_API_KEY=") || hasEnvPrefix(invoked.Env, "CODEX_API_KEY=") {
		t.Fatalf("task check inherited an API secret: %q", invoked.Env)
	}
	if strings.Contains(result.Output, ".agentmux-test-") {
		t.Fatalf("private path leaked to output: %q", result.Output)
	}
	entries, err := os.ReadDir(checkRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".agentmux-test-") {
			t.Fatalf("temporary test directory was not removed: %s", entry.Name())
		}
	}
}

func hasEnvPrefix(env []string, prefix string) bool {
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func TestMakePrivateDirsRejectsRoot(t *testing.T) {
	if _, err := user.Lookup("root"); err != nil {
		t.Skip("root account unavailable")
	}
	if _, err := makePrivateDirs(t.TempDir(), "root"); err == nil || !strings.Contains(err.Error(), "may not run as root") {
		t.Fatalf("makePrivateDirs(root) error = %v", err)
	}
}

func TestDefaultTaskTestCommandIsFixed(t *testing.T) {
	cmd := taskTestCommand(context.Background(), currentUsername(t), "test", "./...")
	want := []string{"/usr/bin/nice", "-n", "10", trustedGoPath(), "test", "./..."}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("trusted check argv = %q, want %q", cmd.Args, want)
	}
	if got, want := projectChecks["m-rk/mergentic"], [][]string{{"vet", "./..."}, {"test", "./..."}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mergentic check argv = %q, want %q", got, want)
	}
}

func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}
