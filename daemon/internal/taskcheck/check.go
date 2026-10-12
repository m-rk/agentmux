// Package taskcheck runs the small set of project-approved checks available
// to Codex task sessions. The caller cannot supply a command or filesystem
// path; all execution details are derived from the task's agentmux registry.
package taskcheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Keep the unary result under gRPC's default 4 MiB message limit.
const maxOutputBytes = 3 << 20

var taskCheckEUID = os.Geteuid
var systemdRun = runSystemd
var stopSystemdUnit = stopUnit
var makeTaskCheckTempRoot = func() (string, error) {
	return os.MkdirTemp("/run/agentmux", ".taskcheck-")
}

func runSystemd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemd-run", args...)
	var output boundedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.Bytes(), err
}

func stopUnit(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "/usr/bin/systemctl", "stop", name).Run()
}

// trustedGoPath resolves Go from the daemon's own PATH, never the task user's
// PATH. This prevents a task-local go shim from replacing the fixed check.
func trustedGoPath() string {
	// Do not search the task user's PATH: an executable placed in a worktree
	// or home directory must not replace the daemon-approved Go tool.
	for _, path := range []string{
		"/usr/local/go/bin/go",
		"/usr/local/bin/go",
		"/opt/homebrew/bin/go",
		"/usr/bin/go",
	} {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o022 == 0 && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return "/usr/bin/go"
}

// Target is resolved from a registered task instance before Run is called.
type Target struct {
	Instance string
	Agent    string
	Project  string
	Workdir  string
	RunUser  string
}

// Result is the complete outcome returned to the invoking task thread.
type Result struct {
	Project         string
	Category        string
	Output          string
	ExitCode        int
	Duration        time.Duration
	OutputTruncated bool
}

// Broker validates the unprivileged caller against the registered task before
// running the existing confined check. readRegistry and userID are injectable
// so the privilege and registry boundaries can be tested without systemd.
func Broker(ctx context.Context, instance, callerUID string,
	readRegistry func(string) (map[string]string, error),
	userID func(string) (string, error), runner func(context.Context, Target) Result,
) Result {
	started := time.Now()
	fail := func(message string) Result {
		return Result{Category: "execution_error", Output: message + "\n", ExitCode: -1, Duration: time.Since(started)}
	}
	if callerUID == "" || callerUID == "0" {
		return fail("task check broker requires a non-root invoking user")
	}
	if !strings.HasPrefix(instance, "task-") || strings.ContainsAny(instance, "/\\\x00\r\n") {
		return fail("project check is available only to task-* instances")
	}
	fields, err := readRegistry(instance)
	if err != nil {
		return fail(fmt.Sprintf("reading task registry: %v", err))
	}
	target, err := Resolve(instance, fields)
	if err != nil {
		return fail(err.Error())
	}
	uid, err := userID(target.RunUser)
	if err != nil {
		return fail(fmt.Sprintf("looking up task user: %v", err))
	}
	if uid != callerUID {
		return fail("task check broker caller does not own the registered task")
	}
	if runner == nil {
		runner = Run
	}
	return runner(ctx, target)
}

// Resolve validates the fields read from an instance registry. Only the
// project check implemented below is available; caller-supplied commands,
// arguments, environment paths and workdirs are deliberately absent.
func Resolve(instance string, fields map[string]string) (Target, error) {
	if !strings.HasPrefix(instance, "task-") || strings.ContainsAny(instance, "/\\\x00\r\n") {
		return Target{}, fmt.Errorf("project check is available only to task-* instances")
	}
	if fields["AGENTMUX_INSTANCE_NAME"] != instance {
		return Target{}, fmt.Errorf("registry identity does not match instance %q", instance)
	}
	if fields["AGENTMUX_AGENT"] != "codex" {
		return Target{}, fmt.Errorf("project check is available only to Codex task instances")
	}
	project := strings.TrimSpace(fields["AGENTMUX_PROJECT"])
	if !approvedProject(project) {
		return Target{}, fmt.Errorf("no trusted full test command is configured for project %q", project)
	}
	workdir := strings.TrimSpace(fields["AGENTMUX_WORKDIR"])
	if !filepath.IsAbs(workdir) || strings.ContainsAny(workdir, "\x00\r\n") {
		return Target{}, fmt.Errorf("task registry has no valid absolute workdir")
	}
	runUser := strings.TrimSpace(fields["AGENTMUX_RUN_USER"])
	if runUser == "" {
		return Target{}, fmt.Errorf("task registry has no run user")
	}
	return Target{Instance: instance, Agent: "codex", Project: project, Workdir: filepath.Clean(workdir), RunUser: runUser}, nil
}

func approvedProject(project string) bool {
	_, ok := projectChecks[project]
	return ok
}

// projectChecks is the reviewed check command for each supported project.
// Entries are argv arrays, never shell text. Keep in sync with each project's
// approved baseline check: agentmux's full Go tests, and mergentic's go vet
// plus full tests.
var projectChecks = map[string][][]string{
	"m-rk/agentmux":  {{"test", "./..."}},
	"m-rk/mergentic": {{"vet", "./..."}, {"test", "./..."}},
}

// Run executes the approved check in a root-managed system unit. The task
// user receives a read-only bind of its checkout and one private writable
// directory for home, temp files and the Go cache. There is no unsandboxed
// fallback when systemd isolation is unavailable.
func Run(ctx context.Context, target Target) Result {
	started := time.Now()
	result := Result{Project: target.Project, ExitCode: -1}
	checks, ok := projectChecks[target.Project]
	if !ok {
		result.Category = "execution_error"
		result.Output = fmt.Sprintf("no trusted check configured for project %q\n", target.Project)
		result.Duration = time.Since(started)
		return result
	}
	if taskCheckEUID() != 0 {
		result.Category = "execution_error"
		result.Output = "project checks require the root-managed system service sandbox\n"
		result.Duration = time.Since(started)
		return result
	}
	worktree, workdir, err := validateWorkdir(target)
	if err != nil {
		result.Category = "execution_error"
		result.Output = err.Error() + "\n"
		result.Duration = time.Since(started)
		return result
	}
	tmpRoot, err := makePrivateDirs(target.RunUser)
	if err != nil {
		result.Category = "execution_error"
		result.Output = err.Error() + "\n"
		result.Duration = time.Since(started)
		return result
	}
	defer os.RemoveAll(tmpRoot)
	privatePath := "/tmp/agentmux-taskcheck"
	cacheDir, tempDir := filepath.Join(privatePath, "cache"), filepath.Join(privatePath, "tmp")

	// nice and every Go argv are fixed by projectChecks. GOPROXY=off and
	// GOTOOLCHAIN=local prevent downloads during the check.
	var output boundedBuffer
	for _, args := range checks {
		commandArgs, argErr := sandboxArgs(target, worktree, workdir, tmpRoot, privatePath, args, cacheDir, tempDir)
		if argErr != nil {
			result.Category = "execution_error"
			result.Output = argErr.Error() + "\n"
			result.Duration = time.Since(started)
			return result
		}
		chunk, runErr := systemdRun(ctx, commandArgs...)
		_, _ = output.Write(chunk)
		err = runErr
		if ctx.Err() != nil {
			for _, arg := range commandArgs {
				if strings.HasPrefix(arg, "--unit=") {
					stopSystemdUnit(strings.TrimPrefix(arg, "--unit="))
					break
				}
			}
		}
		if err != nil {
			break
		}
	}
	result.Output, result.OutputTruncated = output.String(), output.Truncated()
	result.Duration = time.Since(started)
	if err == nil {
		result.ExitCode, result.Category = 0, "passed"
		return result
	}
	if ctx.Err() != nil {
		result.ExitCode, result.Category = 124, "execution_error"
		result.Output += "test check interrupted: " + ctx.Err().Error() + "\n"
		return result
	}
	var exitErr *exec.ExitError
	if unitStartupRE.MatchString(result.Output) {
		result.Category = "execution_error"
		result.Output += "the root-managed systemd sandbox did not start; no unsandboxed check was run\n"
		return result
	}
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	} else {
		result.Output += "starting isolated project check: " + err.Error() + "\n"
		result.Category = "execution_error"
		return result
	}
	result.Category = Classify(result.Output)
	return result
}

func sandboxArgs(target Target, worktree, workdir, tmpRoot, privatePath string, check []string, cacheDir, tempDir string) ([]string, error) {
	unitID := make([]byte, 8)
	if _, err := rand.Read(unitID); err != nil {
		return nil, fmt.Errorf("creating isolated check unit ID: %w", err)
	}
	for _, path := range []string{worktree, workdir, tmpRoot} {
		if strings.ContainsAny(path, ":\\\x00\r\n") {
			return nil, fmt.Errorf("task paths contain characters unsupported by systemd bind properties")
		}
	}
	unitName := "agentmuxd-taskcheck-" + hex.EncodeToString(unitID) + ".service"
	args := []string{
		"--system", "--wait", "--pipe", "--collect", "--quiet", "--unit=" + unitName,
		"--property=Type=exec",
		"--property=User=" + target.RunUser,
		"--property=Nice=10",
		"--property=PrivateNetwork=yes",
		"--property=PrivateTmp=yes",
		"--property=ProtectHome=tmpfs",
		"--property=ProtectSystem=strict",
		"--property=InaccessiblePaths=/run",
		"--property=BindReadOnlyPaths=" + worktree,
		"--property=BindPaths=" + tmpRoot + ":" + privatePath,
		"--property=NoNewPrivileges=yes",
		"--property=PrivateDevices=yes",
		"--property=ProtectKernelTunables=yes",
		"--property=ProtectKernelModules=yes",
		"--property=ProtectControlGroups=yes",
		"--property=RestrictSUIDSGID=yes",
		"--property=CapabilityBoundingSet=",
		"--property=AmbientCapabilities=",
		"--property=WorkingDirectory=" + workdir,
		"--setenv=HOME=" + privatePath + "/home",
		"--setenv=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		"--setenv=GOCACHE=" + cacheDir,
		"--setenv=GOTMPDIR=" + tempDir,
		"--setenv=TMPDIR=" + tempDir,
		"--setenv=GOPROXY=off",
		"--setenv=GOTOOLCHAIN=local",
		"--setenv=GOENV=off",
		"--setenv=GOFLAGS=",
		"--setenv=GOWORK=off",
		"--", trustedGoPath(),
	}
	args = append(args, check...)
	return args, nil
}

func validateWorkdir(target Target) (string, string, error) {
	worktree, err := filepath.EvalSymlinks(target.Workdir)
	if err != nil {
		return "", "", fmt.Errorf("resolving task worktree: %w", err)
	}
	projectDir := worktree
	switch target.Project {
	case "m-rk/agentmux":
		projectDir = filepath.Join(projectDir, "daemon")
	case "m-rk/mergentic":
		// Its Go module is at the registered checkout root.
	default:
		return "", "", fmt.Errorf("no trusted check directory configured for project %q", target.Project)
	}
	workdir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", "", fmt.Errorf("resolving task project directory: %w", err)
	}
	rel, err := filepath.Rel(worktree, workdir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("configured project directory escapes the assigned task worktree")
	}
	info, err := os.Stat(workdir)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("task worktree is unavailable")
	}
	goMod, err := os.Stat(filepath.Join(workdir, "go.mod"))
	if err != nil || !goMod.Mode().IsRegular() {
		return "", "", fmt.Errorf("approved Go project check requires go.mod in its configured project directory")
	}
	return worktree, workdir, nil
}

func makePrivateDirs(runUser string) (string, error) {
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", fmt.Errorf("looking up task user: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return "", fmt.Errorf("invalid task user uid: %w", err)
	}
	if uid == 0 {
		return "", fmt.Errorf("project checks may not run as root")
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return "", fmt.Errorf("invalid task user gid: %w", err)
	}
	root, err := makeTaskCheckTempRoot()
	if err != nil {
		return "", fmt.Errorf("creating private test directory: %w", err)
	}
	cleanup := func(err error) (string, error) { _ = os.RemoveAll(root); return "", err }
	// Keep the bind source itself root-owned so the task user cannot replace
	// it with a symlink between validation and systemd's mount setup.
	if err := os.Chmod(root, 0o711); err != nil {
		return cleanup(fmt.Errorf("securing private test directory: %w", err))
	}
	for _, name := range []string{"cache", "tmp", "home"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			return cleanup(fmt.Errorf("creating private test %s: %w", name, err))
		}
	}
	for _, name := range []string{"cache", "tmp", "home"} {
		if err := os.Chown(filepath.Join(root, name), uid, gid); err != nil {
			return cleanup(fmt.Errorf("assigning private test %s to task user: %w", name, err))
		}
	}
	return root, nil
}

// Classify reports known host sandbox denials separately from ordinary test
// failures. A simultaneous denial and a non-denial go test error is mixed.
func Classify(output string) string {
	denied := sandboxDenialRE.MatchString(output)
	assertion := testAssertionRE.MatchString(output)
	switch {
	case denied && assertion:
		return "mixed_failure"
	case denied:
		return "sandbox_denial"
	default:
		return "test_failure"
	}
}

var (
	sandboxDenialRE = regexp.MustCompile(`(?i)(operation not permitted|permission denied|read-only file system|network is unreachable|listen tcp[^\n]*operation not permitted|failed to create[^\n]*(namespace|network))`)
	unitStartupRE   = regexp.MustCompile(`(?i)(failed to (start|connect to|set) (transient )?(service|unit|bus|properties)|job for [^\n]+ failed)`)
	testAssertionRE = regexp.MustCompile(`(?m)(?:[^\n]+\.go:\d+:\d+:\s+\S|_test\.go:\d+:\s+[^\n]*(?:want|got|expected|unexpected|assert|mismatch)|\b(Error|Errorf|Fatal|Fatalf):)`)
)

type boundedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutputBytes - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	if len(p) < n {
		b.truncated = true
	}
	return n, nil
}

func (b *boundedBuffer) Truncated() bool { return b.truncated }
