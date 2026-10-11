package taskcheck

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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

	oldUID, oldSystemd, oldStop, oldTempRoot := taskCheckEUID, systemdRun, stopSystemdUnit, makeTaskCheckTempRoot
	taskCheckEUID = func() int { return 0 }
	tempBase := t.TempDir()
	makeTaskCheckTempRoot = func() (string, error) { return os.MkdirTemp(tempBase, ".agentmux-taskcheck-") }
	var gotArgs []string
	t.Cleanup(func() {
		taskCheckEUID, systemdRun, stopSystemdUnit, makeTaskCheckTempRoot = oldUID, oldSystemd, oldStop, oldTempRoot
	})
	// An ExitError is the systemd-run result for a non-zero service exit.
	systemdRun = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("stdout check\nstderr detail\n"), exitError(7)
	}
	result := Run(context.Background(), target)
	if result.ExitCode != 7 || result.Category != "test_failure" || !strings.Contains(result.Output, "stdout check") || !strings.Contains(result.Output, "stderr detail") {
		t.Fatalf("Run result = %+v", result)
	}
	checkRoot := filepath.Join(root, "daemon")
	if !reflect.DeepEqual(gotArgs[len(gotArgs)-4:], []string{"--", trustedGoPath(), "test", "./..."}) {
		t.Fatalf("fixed check argv = %q", gotArgs)
	}
	for _, property := range []string{
		"--system", "--property=User=" + target.RunUser, "--property=Nice=10",
		"--property=PrivateNetwork=yes", "--property=ProtectHome=tmpfs",
		"--property=PrivateTmp=yes", "--property=ProtectSystem=strict",
		"--property=InaccessiblePaths=/run", "--property=BindReadOnlyPaths=" + root,
		"--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet=",
		"--property=AmbientCapabilities=", "--property=BindPaths=",
	} {
		found := slices.Contains(gotArgs, property)
		if strings.HasSuffix(property, "=") {
			for _, arg := range gotArgs {
				found = found || strings.HasPrefix(arg, property)
			}
		}
		if !found {
			t.Errorf("sandbox invocation missing %q: %v", property, gotArgs)
		}
	}
	var privateRoot string
	for _, arg := range gotArgs {
		if strings.HasPrefix(arg, "--property=BindPaths=") {
			privateRoot = strings.TrimSuffix(strings.TrimPrefix(arg, "--property=BindPaths="), ":/tmp/agentmux-taskcheck")
		}
	}
	if privateRoot == "" {
		t.Fatal("sandbox invocation did not expose a private writable directory")
	}
	if _, err := os.Stat(privateRoot); !os.IsNotExist(err) {
		t.Fatalf("private bind source was not cleaned up: %q, %v", privateRoot, err)
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

func TestRunFailsClosedWhenRootManagedSandboxCannotStart(t *testing.T) {
	worktree := t.TempDir()
	projectDir := filepath.Join(worktree, "daemon")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module example.test/check\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldUID, oldSystemd, oldStop, oldTempRoot := taskCheckEUID, systemdRun, stopSystemdUnit, makeTaskCheckTempRoot
	taskCheckEUID = func() int { return 0 }
	tempBase := t.TempDir()
	makeTaskCheckTempRoot = func() (string, error) { return os.MkdirTemp(tempBase, ".agentmux-taskcheck-") }
	called := 0
	systemdRun = func(context.Context, ...string) ([]byte, error) {
		called++
		return []byte("Failed to start transient service unit: systemd property unavailable"), exitError(1)
	}
	t.Cleanup(func() {
		taskCheckEUID, systemdRun, stopSystemdUnit, makeTaskCheckTempRoot = oldUID, oldSystemd, oldStop, oldTempRoot
	})
	target := Target{Project: "m-rk/agentmux", Workdir: worktree, RunUser: currentUsername(t)}
	result := Run(context.Background(), target)
	if called != 1 || result.Category != "execution_error" || result.ExitCode != -1 || !strings.Contains(result.Output, "no unsandboxed check was run") {
		t.Fatalf("failed sandbox result = %+v, calls=%d", result, called)
	}
}

func TestUnitStartupFailuresAreExecutionErrors(t *testing.T) {
	for _, output := range []string{
		"Failed to start transient service unit: namespace unavailable",
		"Failed to set unit properties: property unsupported",
		"Failed to connect to bus: access denied",
	} {
		if !unitStartupRE.MatchString(output) {
			t.Errorf("unit startup failure not recognized: %q", output)
		}
	}
}

func TestRunRefusesToExecuteWithoutRootManagedDaemon(t *testing.T) {
	oldUID, oldSystemd := taskCheckEUID, systemdRun
	taskCheckEUID = func() int { return 1000 }
	called := false
	systemdRun = func(context.Context, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() { taskCheckEUID, systemdRun = oldUID, oldSystemd })
	result := Run(context.Background(), Target{Project: "m-rk/agentmux"})
	if called || result.Category != "execution_error" || !strings.Contains(result.Output, "root-managed system service sandbox") {
		t.Fatalf("unprivileged daemon result = %+v, called=%t", result, called)
	}
}

func exitError(code int) error {
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		panic("failed to construct test exit error")
	}
	return exitErr
}

func TestMakePrivateDirsRejectsRoot(t *testing.T) {
	if _, err := user.Lookup("root"); err != nil {
		t.Skip("root account unavailable")
	}
	if _, err := makePrivateDirs("root"); err == nil || !strings.Contains(err.Error(), "may not run as root") {
		t.Fatalf("makePrivateDirs(root) error = %v", err)
	}
}

func TestProjectChecksAreFixed(t *testing.T) {
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
