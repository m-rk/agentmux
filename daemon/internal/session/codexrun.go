package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// Codex headless runs follow the amp pattern (docs/design/codex-runner.md
// section 3): `codex exec --json` runs detached in the worktree, its JSONL
// stream is appended to a log under the run user's state dir, and the run
// state is read back from that log. Continuing a thread is
// `codex exec resume <thread_id>`, which appends a fresh thread.started to
// the same log, so a log is a list of segments exactly like amp's.

// CodexUnsafeSandboxEnv is the registry key an operator sets to 1 to let an
// instance run with sandbox mode danger-full-access. Without it that mode
// (and the approvals/sandbox bypass flag) is refused.
const CodexUnsafeSandboxEnv = "AGENTMUX_CODEX_ALLOW_UNSAFE_SANDBOX"

// DefaultCodexSandbox is the sandbox mode a run uses when none is named:
// the design's recommended workspace-write.
const DefaultCodexSandbox = "workspace-write"

// codexRunStartTimeout bounds waiting for thread.started, which codex
// prints right after launch; a missing one means the CLI failed (bad model,
// no login), so the run is refused instead of returning a phantom thread.
const codexRunStartTimeout = 60 * time.Second

// CodexRunStalledAfter is how long a running codex log may go without an
// append before it is reported stalled (the amp threshold).
const CodexRunStalledAfter = AmpRunStalledAfter

// codexThreadID is the shape of a codex thread id (a UUID).
var codexThreadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidCodexThreadID reports whether id has the shape of a codex thread id.
func ValidCodexThreadID(id string) bool { return codexThreadID.MatchString(id) }

var (
	codexModelRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,127}$`)
	codexEffortRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// CodexRunStateDir is ~/.local/state/agentmux/sessions/<instance>, shared
// with the amp run logs.
func CodexRunStateDir(home, instance string) string {
	return ampRunStateDir(home, instance)
}

// CodexRunLogPath is the stream log for one run: per thread once the id is
// known, or a pending path until then (see AmpRunLogPath).
func CodexRunLogPath(home, instance, thread string) string {
	name := "codex-run-pending.jsonl"
	if thread != "" {
		name = "codex-run-" + thread + ".jsonl"
	}
	return filepath.Join(CodexRunStateDir(home, instance), name)
}

// CodexRunOptions are the per-run knobs. Model and Effort are free strings
// passed through (shape-checked only; no model names live in code).
type CodexRunOptions struct {
	Workdir string
	Model   string
	Effort  string
	// Sandbox is read-only, workspace-write or danger-full-access; empty
	// means DefaultCodexSandbox.
	Sandbox string
	// AllowUnsafe is the instance's explicit opt-in (CodexUnsafeSandboxEnv)
	// for danger-full-access.
	AllowUnsafe bool
}

// CodexUnsafeArg reports why one codex argument bypasses the sandbox, or ""
// if it does not: the bypass flags, `-s danger-full-access`, or a `-c`
// override of sandbox_mode/approval_policy that would do the same.
func codexUnsafeArg(args []string, i int) string {
	a := args[i]
	next := ""
	if i+1 < len(args) {
		next = args[i+1]
	}
	switch {
	case a == "--dangerously-bypass-approvals-and-sandbox" || a == "--yolo":
		return a
	case a == "-s" || a == "--sandbox":
		if strings.TrimSpace(next) == "danger-full-access" {
			return a + " danger-full-access"
		}
	case strings.HasPrefix(a, "--sandbox="):
		if strings.TrimPrefix(a, "--sandbox=") == "danger-full-access" {
			return a
		}
	case a == "-c" || a == "--config":
		return codexUnsafeConfig(next)
	case strings.HasPrefix(a, "--config="):
		return codexUnsafeConfig(strings.TrimPrefix(a, "--config="))
	}
	return ""
}

func codexUnsafeConfig(kv string) string {
	k, v, _ := strings.Cut(kv, "=")
	k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
	if k == "sandbox_mode" && v == "danger-full-access" {
		return "-c " + kv
	}
	return ""
}

// CheckCodexArgs refuses argv that bypasses the sandbox unless allowUnsafe.
// It is the last guard in CodexRunArgs, so no caller can smuggle a bypass
// through the model or effort strings.
func CheckCodexArgs(args []string, allowUnsafe bool) error {
	if allowUnsafe {
		return nil
	}
	for i := range args {
		if why := codexUnsafeArg(args, i); why != "" {
			return fmt.Errorf("codex %s is refused: sandbox bypass needs %s=1 on the instance", why, CodexUnsafeSandboxEnv)
		}
	}
	return nil
}

// CleanCodexSandbox resolves the sandbox mode for a run: empty gives the
// default, an unknown value is refused, and danger-full-access needs the
// instance's explicit opt-in.
func CleanCodexSandbox(mode string, allowUnsafe bool) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "":
		return DefaultCodexSandbox, nil
	case "read-only", "workspace-write":
		return mode, nil
	case "danger-full-access":
		if !allowUnsafe {
			return "", fmt.Errorf("sandbox danger-full-access is refused: set %s=1 on the instance to allow it", CodexUnsafeSandboxEnv)
		}
		return mode, nil
	}
	return "", fmt.Errorf("unknown codex sandbox %q (want read-only or workspace-write)", mode)
}

// CleanCodexModel shape-checks a model id; empty means codex's default.
func CleanCodexModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model != "" && !codexModelRE.MatchString(model) {
		return "", fmt.Errorf("codex model %q has an unusable shape", model)
	}
	return model, nil
}

// CleanCodexEffort shape-checks a reasoning effort; empty means the
// model's default. Valid levels are per-model, so codex itself judges them.
func CleanCodexEffort(effort string) (string, error) {
	effort = strings.TrimSpace(effort)
	if effort != "" && !codexEffortRE.MatchString(effort) {
		return "", fmt.Errorf("codex effort %q has an unusable shape", effort)
	}
	return effort, nil
}

// CodexRunArgs builds the `codex` argv (without the binary) for a run: a new
// thread (threadID "") or `exec resume <threadID>`. The prompt is never in
// argv: the trailing `-` makes codex read it from stdin, which the launcher
// points at a file. Exec-level flags (-C, -s) come before `resume` because
// the resume subcommand does not take them.
func CodexRunArgs(opts CodexRunOptions, threadID string) ([]string, error) {
	sandbox, err := CleanCodexSandbox(opts.Sandbox, opts.AllowUnsafe)
	if err != nil {
		return nil, err
	}
	model, err := CleanCodexModel(opts.Model)
	if err != nil {
		return nil, err
	}
	effort, err := CleanCodexEffort(opts.Effort)
	if err != nil {
		return nil, err
	}
	if threadID != "" && !ValidCodexThreadID(threadID) {
		return nil, fmt.Errorf("%q is not a codex thread id", threadID)
	}
	args := []string{"exec", "--json"}
	if opts.Workdir != "" {
		args = append(args, "-C", opts.Workdir)
	}
	args = append(args, "-s", sandbox)
	if model != "" {
		args = append(args, "-m", model)
	}
	if effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+effort)
	}
	if threadID != "" {
		args = append(args, "resume", threadID)
	}
	args = append(args, "-")
	if err := CheckCodexArgs(args, opts.AllowUnsafe); err != nil {
		return nil, err
	}
	return args, nil
}

// codexStartNew spawns the detached run child; replaceable in tests.
var codexStartNew = startCodexProcessDetached

// StartCodexRun launches argv detached in workdir with the prompt on stdin
// and output appended to logPath, and returns the thread id once codex
// prints thread.started. The child outlives this process.
func StartCodexRun(ctx context.Context, instance string, argv []string, prompt, workdir, logPath string) (string, error) {
	// A continue appends to a log that already holds earlier segments, so
	// only what lands after this point counts as this run's start.
	var offset int64
	if info, err := os.Stat(logPath); err == nil {
		offset = info.Size()
	}
	proc, err := codexStartNew(ctx, instance, argv, prompt, workdir, logPath)
	if err != nil {
		return "", err
	}
	_ = proc.Release()
	return waitCodexThread(ctx, logPath, offset)
}

// codexRunCommand builds the run child: plain `codex` as the current user
// with the task identity stamped on its environment (see AmpRunEnv).
func codexRunCommand(ctx context.Context, instance string, argv []string) *exec.Cmd {
	cmd := runas.CurrentUserCommandContext(ctx, "codex", argv...)
	cmd.Env = append(cmd.Env, AmpRunEnv(instance)...)
	return cmd
}

func startCodexProcessDetached(ctx context.Context, instance string, argv []string, prompt, workdir, logPath string) (*os.Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("codex run needs a command")
	}
	if instance == "" {
		instance = instanceForRun()
	}
	cmd := codexRunCommand(ctx, instance, argv)
	bin, rest := cmd.Args[0], cmd.Args[1:]
	if !filepath.IsAbs(bin) {
		var err error
		if bin, err = runas.CurrentUserLookPath(bin); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating codex run state dir: %w", err)
	}
	// The prompt lives in a private file next to the log, never in argv
	// (world-readable process table) and never on a pipe that could block.
	// The launcher deletes it once codex has exited.
	promptFile, err := os.CreateTemp(filepath.Dir(logPath), "codex-prompt-*.txt")
	if err != nil {
		return nil, fmt.Errorf("writing codex prompt: %w", err)
	}
	promptPath := promptFile.Name()
	if _, err := promptFile.WriteString(prompt); err != nil {
		promptFile.Close()
		os.Remove(promptPath)
		return nil, fmt.Errorf("writing codex prompt: %w", err)
	}
	if err := promptFile.Close(); err != nil {
		os.Remove(promptPath)
		return nil, fmt.Errorf("writing codex prompt: %w", err)
	}
	// Double fork through sh as for amp: the middle child backgrounds a
	// subshell and exits, so codex is reparented to init. The subshell holds
	// the log open on fd 3 (so ops.Run can rename the pending log to its
	// thread name while codex keeps appending) and brackets codex's output
	// with agentmux.start and agentmux.exit records: the exit record is how
	// the parser tells "exited without a result" from "still working", and
	// the start record begins a segment even if codex dies before printing
	// thread.started. API-key variables are dropped so a run can never
	// silently switch the account to metered billing. Positionals after -c's
	// script: $0=bin, $1=log, $2=prompt file, $3=workdir, $4...=codex args.
	launch := "f=\"$1\"; p=\"$2\"; d=\"$3\"; shift 3; cd \"$d\" || exit 1; unset OPENAI_API_KEY CODEX_API_KEY; " +
		"( exec 3>>\"$f\"; echo '{\"type\":\"agentmux.start\"}' >&3; \"$0\" \"$@\" <\"$p\" >&3 2>&3; c=$?; " +
		"rm -f \"$p\"; echo \"{\\\"type\\\":\\\"agentmux.exit\\\",\\\"code\\\":$c}\" >&3 ) & exit 0"
	midArgs := append([]string{"-c", launch, bin, logPath, promptPath, workdir}, rest...)
	mid := runas.CurrentUserCommandContext(ctx, "sh", midArgs...)
	mid.Env = cmd.Env
	mid.Stdin = strings.NewReader("")
	if err := mid.Start(); err != nil {
		return nil, fmt.Errorf("spawning codex run: %w", err)
	}
	return mid.Process, nil
}

type codexEvent struct {
	Type     string `json:"type"`
	Code     int    `json:"code"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

func waitCodexThread(ctx context.Context, logPath string, offset int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, codexRunStartTimeout)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		id, done, err := scanCodexThread(logPath, offset)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
		if done {
			return "", fmt.Errorf("codex exited before printing thread.started: %s", ampLogTail(logPath))
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("codex printed no thread.started within %s: %s", codexRunStartTimeout, ampLogTail(logPath))
		case <-tick.C:
		}
	}
}

// scanCodexThread reads the log from offset for this run's thread.started,
// or its agentmux.exit record (done: the child exited without one).
func scanCodexThread(logPath string, offset int64) (id string, done bool, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading codex run log %s: %w", logPath, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return "", false, fmt.Errorf("reading codex run log %s: %w", logPath, err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev codexEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				return ev.ThreadID, false, nil
			}
		case "agentmux.exit":
			return "", true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("reading codex run log %s: %w", logPath, err)
	}
	return "", false, nil
}

// CodexRunState is what the JSONL log says about a codex run thread.
type CodexRunState struct {
	// State is "running", "done" or "failed".
	State string `json:"state"`
	// Reason is set when State is "failed".
	Reason string `json:"reason,omitempty"`
	// RateLimited marks a failure caused by a rate or usage limit: retry
	// later, not a broken run.
	RateLimited bool `json:"rate_limited,omitempty"`
	// Stalled marks a running log with no append for CodexRunStalledAfter.
	Stalled  bool   `json:"stalled,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

// CodexRunStateOf reads the log of one run thread. Each thread.started
// begins a segment (a resume appends another), resetting to running; in the
// current segment turn.completed means done and turn.failed means failed
// with the error's message. A bare `error` event is not terminal (codex
// reports transient reconnects that way) but is remembered as the reason if
// the launcher's agentmux.exit record then shows the process gone without a
// terminal event, as is a log tail when there was no error at all. No
// terminal event and no exit record means still running (Stalled once the
// log goes quiet); a missing log is running, since the first run may still
// be spawning.
func CodexRunStateOf(logPath string) CodexRunState {
	st := CodexRunState{State: "running"}
	f, err := os.Open(logPath)
	if err != nil {
		if !os.IsNotExist(err) {
			st.State, st.Reason = "failed", fmt.Sprintf("reading codex run log: %v", err)
		}
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lastErr string
	var exited bool
	var exitCode int
	for sc.Scan() {
		var ev codexEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "agentmux.start":
			st.State, st.Reason, st.RateLimited, lastErr = "running", "", false, ""
			exited = false
		case "thread.started":
			if ev.ThreadID != "" {
				st.ThreadID = ev.ThreadID
			}
			st.State, st.Reason, st.RateLimited, lastErr = "running", "", false, ""
			exited = false
		case "turn.started":
			st.State, st.Reason, st.RateLimited = "running", "", false
		case "turn.completed":
			st.State, st.Reason, st.RateLimited = "done", "", false
		case "turn.failed":
			msg := ""
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			if msg == "" {
				msg = lastErr
			}
			st.State, st.Reason, st.RateLimited = "failed", codexErrorReason(msg), codexRateLimited(msg)
		case "error":
			lastErr = ev.Message
		case "agentmux.exit":
			exited, exitCode = true, ev.Code
		}
	}
	if err := sc.Err(); err != nil {
		st.State, st.Reason, st.RateLimited = "failed", fmt.Sprintf("reading codex run log: %v", err), false
		return st
	}
	if st.State != "running" {
		return st
	}
	if exited {
		st.State = "failed"
		why := fmt.Sprintf("process exited (code %d) without a result", exitCode)
		if lastErr != "" {
			st.Reason, st.RateLimited = why+": "+codexErrorReason(lastErr), codexRateLimited(lastErr)
		} else {
			st.Reason = why + ": " + ampLogTail(logPath)
		}
		return st
	}
	st.Stalled = CodexRunLogStalled(logPath)
	return st
}

// codexErrorReason flattens codex's error message, usually a JSON string
// like {"type":"error","status":429,"error":{"type":..,"message":..}}, into
// "<type> (<status>): <message>", capped for status output.
func codexErrorReason(msg string) string {
	msg = strings.TrimSpace(msg)
	var e struct {
		Status int `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	out := msg
	if json.Unmarshal([]byte(msg), &e) == nil && e.Error.Message != "" {
		out = e.Error.Message
		if e.Error.Type != "" {
			out = e.Error.Type + ": " + out
		}
		if e.Status != 0 {
			out = fmt.Sprintf("%s (status %d)", out, e.Status)
		}
	}
	if out == "" {
		out = "error"
	}
	if len(out) > 300 {
		out = out[:300] + "…"
	}
	return out
}

// codexRateLimited reports whether an error message is a rate or usage
// limit: HTTP 429, a rate_limit/usage_limit error type, or those words.
func codexRateLimited(msg string) bool {
	var e struct {
		Status int `json:"status"`
		Error  struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &e) == nil && (e.Status == 429 || strings.Contains(e.Error.Type, "rate_limit") || strings.Contains(e.Error.Type, "usage_limit")) {
		return true
	}
	l := strings.ToLower(msg)
	return strings.Contains(l, "rate limit") || strings.Contains(l, "rate_limit") || strings.Contains(l, "usage limit")
}

// CodexRunLogStalled reports whether the log exists and was not appended to
// for CodexRunStalledAfter. Only the mtime is read.
func CodexRunLogStalled(logPath string) bool {
	return AmpRunLogStalled(logPath)
}

// StopCodexRuns kills every in-flight codex run child of an instance: the
// `codex exec` processes whose stamped AGENTMUX_INSTANCE_NAME matches and
// whose cwd is workdir. Used before a continue preempts a stalled run and by
// retire before the worktree goes. Best-effort, Linux only.
func StopCodexRuns(instance, workdir string) {
	stopCodexRuns(instance, workdir)
}

var stopCodexRuns = stopCodexRunsImpl

func stopCodexRunsImpl(instance, workdir string) {
	for _, pid := range codexRunPidsForInstance(instance, workdir) {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
}

func codexRunPidsForInstance(instance, workdir string) []int {
	if instance == "" {
		return nil
	}
	matches, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		return nil
	}
	var out []int
	for _, p := range matches {
		if !codexRunCmdlineMatch(p) || !ampRunEnvironMatch(p, instance) {
			continue
		}
		if workdir != "" {
			if link, err := os.Readlink(filepath.Join(filepath.Dir(p), "cwd")); err != nil || link != workdir {
				continue
			}
		}
		var pid int
		if _, err := fmt.Sscanf(filepath.Base(filepath.Dir(p)), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// codexRunCmdlineMatch reports whether a /proc cmdline is a `codex exec`
// process: an argv field whose basename is codex directly followed by exec.
func codexRunCmdlineMatch(cmdlinePath string) bool {
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false
	}
	fields := strings.Split(string(data), "\x00")
	for i := 0; i+1 < len(fields); i++ {
		if filepath.Base(fields[i]) == "codex" && fields[i+1] == "exec" {
			return true
		}
	}
	return false
}
