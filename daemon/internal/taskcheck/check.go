// Package taskcheck runs the small set of project-approved checks available
// to Codex task sessions. The caller cannot supply a command or filesystem
// path; all execution details are derived from the task's agentmux registry.
package taskcheck

import (
	"bytes"
	"context"
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

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// Keep the unary result under gRPC's default 4 MiB message limit.
const maxOutputBytes = 3 << 20

var taskTestCommand = func(ctx context.Context, runUser string, args ...string) *exec.Cmd {
	argv := append([]string{"-n", "10", trustedGoPath()}, args...)
	return runas.CommandContext(ctx, runUser, "/usr/bin/nice", argv...)
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

// Run executes the approved check for the registry project as the registered
// task user. Temporary files and the Go build cache are private to this
// worktree and removed when the synchronous call completes.
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
	workdir, err := validateWorkdir(target)
	if err != nil {
		result.Category = "execution_error"
		result.Output = err.Error() + "\n"
		result.Duration = time.Since(started)
		return result
	}
	tmpRoot, err := makePrivateDirs(workdir, target.RunUser)
	if err != nil {
		result.Category = "execution_error"
		result.Output = err.Error() + "\n"
		result.Duration = time.Since(started)
		return result
	}
	defer os.RemoveAll(tmpRoot)
	cacheDir, tempDir := filepath.Join(tmpRoot, "cache"), filepath.Join(tmpRoot, "tmp")

	// nice and every Go argv are fixed by projectChecks. GOPROXY=off and
	// GOTOOLCHAIN=local prevent downloads during the check.
	var output boundedBuffer
	for _, args := range checks {
		cmd := taskTestCommand(ctx, target.RunUser, args...)
		cmd.Dir = workdir
		cmd.Env = safeEnv(cmd.Env, map[string]string{
			"GOCACHE":     cacheDir,
			"GOTMPDIR":    tempDir,
			"TMPDIR":      tempDir,
			"GOPROXY":     "off",
			"GOTOOLCHAIN": "local",
			"GOENV":       "off",
			"GOFLAGS":     "",
			"GOWORK":      "off",
		})
		cmd.Stdout, cmd.Stderr = &output, &output
		err = cmd.Run()
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
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	} else {
		result.Output += err.Error() + "\n"
		result.Category = "execution_error"
		if ctx.Err() != nil {
			result.ExitCode = 124
		}
		return result
	}
	result.Category = Classify(result.Output)
	return result
}

func validateWorkdir(target Target) (string, error) {
	projectDir := target.Workdir
	switch target.Project {
	case "m-rk/agentmux":
		projectDir = filepath.Join(projectDir, "daemon")
	case "m-rk/mergentic":
		// Its Go module is at the registered checkout root.
	default:
		return "", fmt.Errorf("no trusted check directory configured for project %q", target.Project)
	}
	workdir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolving task worktree: %w", err)
	}
	info, err := os.Stat(workdir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("task worktree is unavailable")
	}
	goMod, err := os.Stat(filepath.Join(workdir, "go.mod"))
	if err != nil || !goMod.Mode().IsRegular() {
		return "", fmt.Errorf("approved Go project check requires go.mod in its configured project directory")
	}
	return workdir, nil
}

func makePrivateDirs(workdir, runUser string) (string, error) {
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
	root, err := os.MkdirTemp(workdir, ".agentmux-test-")
	if err != nil {
		return "", fmt.Errorf("creating private test directory: %w", err)
	}
	cleanup := func(err error) (string, error) { _ = os.RemoveAll(root); return "", err }
	for _, name := range []string{"cache", "tmp"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			return cleanup(fmt.Errorf("creating private test %s: %w", name, err))
		}
	}
	if os.Geteuid() != uid {
		if err := os.Chown(root, uid, gid); err != nil {
			return cleanup(fmt.Errorf("assigning private test directory to task user: %w", err))
		}
		for _, name := range []string{"cache", "tmp"} {
			if err := os.Chown(filepath.Join(root, name), uid, gid); err != nil {
				return cleanup(fmt.Errorf("assigning private test %s to task user: %w", name, err))
			}
		}
	}
	return root, nil
}

func safeEnv(env []string, overrides map[string]string) []string {
	base := map[string]string{}
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if key == "HOME" || key == "PATH" || key == "LANG" || strings.HasPrefix(key, "LC_") {
			_, value, _ := strings.Cut(item, "=")
			base[key] = value
		}
	}
	for key, value := range overrides {
		base[key] = value
	}
	var out []string
	for _, key := range []string{"HOME", "PATH", "LANG", "LC_ALL", "GOCACHE", "GOTMPDIR", "TMPDIR", "GOPROXY", "GOTOOLCHAIN", "GOENV", "GOFLAGS", "GOWORK"} {
		if value, ok := base[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	return out
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
