package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// AmpThreadURL is the public address of one amp thread.
const ampThreadURLPrefix = "https://ampcode.com/threads/"

// ampStreamInit is the first record `amp -x --stream-json` prints, whose
// session_id is the thread id `run` returns.
type ampStreamInit struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

// ampRunStartTimeout bounds waiting for the init record: amp prints it
// immediately on launch, so a missing one means the CLI failed (bad mode,
// no auth) rather than a slow agent — the run is refused instead of
// returning a thread that will never exist.
const ampRunStartTimeout = 60 * time.Second

// maxAmpModeBytes bounds a mode key/label. amp documents 24 characters for
// plugin modes; 256 leaves headroom for future built-ins without letting
// a garbage registry value ride along in argv unbounded.
const maxAmpModeBytes = 256

// AmpRunLogPath is the stream log for one run: per thread once the
// thread id is known, or a pending path until then. home is the run
// user's home; callers rename pending to threaded after the init record
// arrives.
func AmpRunLogPath(home, instance, thread string) string {
	name := "amp-run-pending.jsonl"
	if thread != "" {
		name = "amp-run-" + thread + ".jsonl"
	}
	return filepath.Join(ampRunStateDir(home, instance), name)
}

// ampRunStateDir is ~/.local/state/agentmux/sessions/<instance>, holding
// the stream logs. It parallels safesend's audit path: per-user state
// under the run user's home, not the registry.
func ampRunStateDir(home, instance string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "sessions", instance)
}

// ampRunArgs builds the `amp` argv (without the binary itself) for
// starting (threadID "") or continuing a thread. Exported as AmpRunArgs so
// ops.Run builds the same argv the session layer spawns. Flags come before
// `-x` and the prompt goes last as -x's message: `-x` consumes the next
// argument as its message even when it names a flag, so `-x --stream-json
// "prompt"` fails (confirmed live 2026-10-05) while `--stream-json -x
// "prompt"` works. Piped stdin would arrive as extra "Input received on
// stdin" context text alongside the message (confirmed live), so the
// prompt rides argv instead. When mode is set it is passed as -m, since
// `-m "<label>"` runs the mode's own model and provider (confirmed live
// 2026-10-05). With no mode there is no -m at all and amp uses whatever
// it would by default. A title names a new thread only (`--title` plus
// `--no-archive-after-execute`, so the finished thread stays findable and
// renamable); continuing a thread ignores it.
//
// Returned as an argv slice, not a shell string, so a mode label or title
// with spaces needs no quoting.
func AmpRunArgs(message, mode, threadID, title string) []string {
	var args []string
	if threadID == "" {
		args = []string{"--stream-json"}
	} else {
		args = []string{"threads", "continue", threadID, "--stream-json"}
	}
	if mode != "" {
		args = append(args, "-m", mode)
	}
	if threadID == "" && title != "" {
		args = append(args, "--title", title, "--no-archive-after-execute")
	}
	return append(args, "-x", message)
}

// ampRunCommand builds the detached child: plain `amp` normally, or `op
// run` with the instance's env-file when one exists (see openv.go), so
// AMP_API_KEY resolves only in the child's environment. The service
// account token rides the child's environment and is stripped again by
// `env -u` before amp starts, the same shape as ExecAmp — except the
// runner re-enters agentmux through `session exec` because tmux needs a
// plain argv, while here agentmux spawns the child directly and can set
// its environment itself.
func ampRunCommand(ctx context.Context, envFile string, argv []string) (*exec.Cmd, error) {
	if envFile == "" {
		return runas.CurrentUserCommandContext(ctx, "amp", argv...), nil
	}
	tok, err := readOpToken()
	if err != nil {
		return nil, err
	}
	if _, err := runas.CurrentUserLookPath("op"); err != nil {
		return nil, fmt.Errorf("1Password CLI: %w", err)
	}
	cmd := runas.CurrentUserCommandContext(ctx, "op",
		opRunArgs(envFile, append([]string{"amp"}, argv...))...)
	cmd.Env = append(cmd.Env, opTokenEnv+"="+tok)
	return cmd, nil
}

// ampStartNew spawns the run child detached: argv resolved through
// ampRunCommand (plain `amp`, or `op run` with the instance's env-file),
// workdir as its directory, stdout/stderr appended to logPath, stdin empty
// (with stdin open amp waits on it and fails — see transcript.ampCommand).
// Detached means a double fork: the middle child exits immediately so the
// grandchild is reparented to init and survives this process; the caller
// releases the middle child, never the agent itself. A ".done" sentinel
// appears at logPath+".done" once the agent exits, which is how
// scanAmpInit tells "no init yet" from "exited without init".
// Replaceable in tests (this package swaps ampStartNew/checkAmpMode
// directly; ops tests use the Amp*ForTest helpers below since unexported
// vars don't cross packages).
var ampStartNew = startAmpProcessDetached

// StartAmpRun launches argv detached in workdir with output to logPath
// and returns once the init record arrives. argv holds the amp arguments
// (AmpRunArgs shape, without the binary); the binary resolves through
// ampRunCommand, so an instance env-file runs amp under `op run` exactly
// like the mode probe. The child outlives this process: callers report
// the thread while the agent keeps working.
func StartAmpRun(ctx context.Context, envFile string, argv []string, workdir, logPath string) (string, error) {
	proc, err := ampStartNew(ctx, envFile, argv, workdir, logPath)
	if err != nil {
		return "", err
	}
	// The process is detached by design (it keeps working after run
	// returns), so release it rather than waiting: its output goes to
	// the log file, never a pipe that could fill and block it.
	_ = proc.Release()
	return waitAmpInit(ctx, logPath)
}

func startAmpProcessDetached(ctx context.Context, envFile string, argv []string, workdir, logPath string) (*os.Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("amp run needs a command")
	}
	// Resolve through ampRunCommand, not a bare PATH lookup: with an
	// instance env-file the child is `op run --env-file=... --
	// /usr/bin/env -u ... amp <args>`, and cmd.Path/Args carry the full
	// spawn shape (resolved binary plus env) the double-fork replays.
	cmd, err := ampRunCommand(ctx, envFile, argv)
	if err != nil {
		return nil, err
	}
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("amp run needs a command")
	}
	bin, rest := cmd.Args[0], cmd.Args[1:]
	if !filepath.IsAbs(bin) {
		bin, err = runas.CurrentUserLookPath(bin)
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating amp run state dir: %w", err)
	}
	// Double fork through sh: the middle child starts the agent in the
	// background with the log as its stdout/stderr and exits at once, so
	// the agent is reparented to init and survives this process. argv
	// after the command are passed positionally — never interpolated — so
	// a prompt, title, or mode label with spaces or quotes can't break
	// the shell. Log path and workdir are absolute (AmpRunLogPath joins
	// the run user's home), so the script uses them as given: no
	// `$(pwd)` prefix, which would double the path and write the log
	// somewhere waitAmpInit never looks. Positional order after -c's
	// script is: $0=bin, $1=log, $2=workdir, $3...=agent args.
	launch := "f=\"$1\"; d=\"$2\"; shift 2; cd \"$d\" || exit 1; \"$0\" \"$@\" >>\"$f\" 2>&1 & pid=$!; wait $pid; code=$?; [ $code -ne 0 ] && touch \"$f.done\"; exit 0"
	midArgs := append([]string{"-c", launch, bin, logPath, workdir}, rest...)
	mid := runas.CurrentUserCommandContext(ctx, "sh", midArgs...)
	// Inherit the resolved spawn environment (notably OP_SERVICE_ACCOUNT_TOKEN
	// under `op run`, stripped again by `env -u` before amp starts): the
	// middle child is plain sh, so without this the grandchild would lose
	// the `op` token and the instance's secret references.
	mid.Env = cmd.Env
	// Empty stdin, like every other amp spawn here: with stdin open amp
	// waits on it and fails.
	mid.Stdin = strings.NewReader("")
	if err := mid.Start(); err != nil {
		return nil, fmt.Errorf("spawning amp run: %w", err)
	}
	return mid.Process, nil
}

// waitAmpInit polls logPath for the stream init record and returns its
// session_id. A child that exits first, or never prints init within
// ampRunStartTimeout, is a failed launch (bad mode, no auth): its stderr
// tail becomes the refusal reason. A bogus -m fails in seconds (confirmed
// live ~8s), so the 60s bound is only for a hung launch, not the
// mode-rejection path.
func waitAmpInit(ctx context.Context, logPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ampRunStartTimeout)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		id, done, err := scanAmpInit(logPath)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
		if done {
			return "", fmt.Errorf("amp exited before printing its init record: %s", ampLogTail(logPath))
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("amp printed no init record within %s: %s", ampRunStartTimeout, ampLogTail(logPath))
		case <-tick.C:
		}
	}
}

// AmpRunState describes what the stream log says about a run thread:
// running (the agent hasn't finished), done, or failed with a reason.
type AmpRunState struct {
	// State is "running", "done", or "failed".
	State string `json:"state"`
	// Reason is set when State is "failed": the tail of the log.
	Reason string `json:"reason,omitempty"`
	// ThreadID is the thread the log belongs to.
	ThreadID string `json:"thread_id,omitempty"`
}

// AmpRunStateOf reads the stream log for a run thread. A result record
// ends the run: subtype "success" means done, anything else (or a .done
// sentinel from a launch failure) means failed. No result record means the
// agent is still working — including a log that doesn't exist yet, since
// the first run may still be spawning.
func AmpRunStateOf(logPath string) AmpRunState {
	st := AmpRunState{State: "running"}
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return st
		}
		st.State, st.Reason = "failed", fmt.Sprintf("reading amp run log: %v", err)
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lastID string
	for sc.Scan() {
		line := sc.Bytes()
		var init ampStreamInit
		if json.Unmarshal(line, &init) == nil && init.Type == "system" && init.Subtype == "init" && init.SessionID != "" {
			lastID = init.SessionID
			continue
		}
		var res struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if json.Unmarshal(line, &res) != nil || res.Type != "result" {
			continue
		}
		st.ThreadID = lastID
		switch {
		case res.Subtype == "success" && !res.IsError:
			st.State = "done"
		default:
			st.State, st.Reason = "failed", ampResultReason(res.Subtype, res.Result)
		}
	}
	if err := sc.Err(); err != nil {
		st.State, st.Reason = "failed", fmt.Sprintf("reading amp run log: %v", err)
		return st
	}
	st.ThreadID = lastID
	if st.State == "running" {
		if _, serr := os.Stat(logPath + ".done"); serr == nil {
			st.State, st.Reason = "failed", ampLogTail(logPath)
		}
	}
	return st
}

func ampResultReason(subtype, result string) string {
	r := strings.TrimSpace(result)
	if len(r) > 300 {
		r = r[:300] + "…"
	}
	if subtype != "" && subtype != "success" {
		if r != "" {
			return subtype + ": " + r
		}
		return subtype
	}
	if r != "" {
		return r
	}
	return "error"
}

// scanAmpInit reads the init record from logPath: the thread id, or "" if
// it hasn't arrived yet. done reports the child already exited —
// approximated by a ".done" sentinel the spawner writes after wait, since
// the released process can't be waited on here.
func scanAmpInit(logPath string) (id string, done bool, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading amp run log %s: %w", logPath, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var init ampStreamInit
		if json.Unmarshal(sc.Bytes(), &init) != nil {
			continue
		}
		if init.Type == "system" && init.Subtype == "init" && init.SessionID != "" {
			return init.SessionID, false, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("reading amp run log %s: %w", logPath, err)
	}
	if _, serr := os.Stat(logPath + ".done"); serr == nil {
		return "", true, nil
	}
	return "", false, nil
}

// ampLogTail is the last few lines of the log: what a failed launch
// printed instead of an init record (amp's mode rejection lands on
// stderr, which the spawner redirects to the same log).
func ampLogTail(logPath string) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	s := strings.Join(lines, " ")
	if len(s) > 500 {
		s = s[len(s)-500:]
	}
	if strings.TrimSpace(s) == "" {
		return "(empty log)"
	}
	return s
}

// AmpFakeSpawn holds what an ops test's fake spawn recorded.
type AmpFakeSpawn struct {
	// Argv is the argv the fake was asked to spawn.
	Argv []string
	// Mode is the mode the probe was asked about.
	Mode string
	// Renamed is the title the fake rename was asked to apply; RenameSeen
	// reports whether a rename ran at all.
	Renamed    string
	RenameSeen bool
}

// AmpSwapForTest substitutes the detached spawn, the mode probe, and the
// rename so ops tests never touch a real amp CLI. The fake spawn records
// argv, writes an init record for threadID to the log, and returns a
// stand-in process; probeErr is the probe's verdict (nil succeeds). The
// fake rename records its title and succeeds. It returns a restore func
// the test defers, and the record the fakes fill in.
func AmpSwapForTest(threadID string, probeErr error) (restore func(), fake *AmpFakeSpawn) {
	fake = &AmpFakeSpawn{}
	oldStart, oldCheck, oldRename := ampStartNew, checkAmpMode, renameAmpThread
	ampStartNew = func(_ context.Context, _ string, argv []string, _ string, logPath string) (*os.Process, error) {
		fake.Argv = append([]string(nil), argv...)
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(logPath, []byte(`{"type":"system","subtype":"init","session_id":"`+threadID+`"}`+"\n"), 0o600); err != nil {
			return nil, err
		}
		return os.FindProcess(os.Getpid())
	}
	checkAmpMode = func(_ context.Context, _ string, mode string) error {
		fake.Mode = mode
		return probeErr
	}
	renameAmpThread = func(_ context.Context, _, _, title string) {
		fake.RenameSeen = true
		fake.Renamed = title
	}
	return func() { ampStartNew, checkAmpMode, renameAmpThread = oldStart, oldCheck, oldRename }, fake
}

// checkAmpMode is the mode probe var: production calls it through
// CheckAmpMode, tests swap it (this package directly, ops through
// AmpSwapForTest).
var checkAmpMode = checkAmpModeImpl

// maxAmpTitleBytes bounds a run title. amp titles are short sidebar
// labels; 256 leaves headroom without letting a garbage caller value ride
// along in argv unbounded.
const maxAmpTitleBytes = 256

// CleanAmpTitle trims a run title and rejects what amp can't use: empty,
// overlong, or carrying line breaks or NUL bytes (argv rides through a sh
// double-fork, so a newline could smuggle a second command). Empty means
// "no title" — not an error — so callers keep one code path for titled
// and untitled runs.
func CleanAmpTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", nil
	}
	if len(title) > maxAmpTitleBytes {
		return "", fmt.Errorf("amp title is %d bytes (limit %d)", len(title), maxAmpTitleBytes)
	}
	if strings.ContainsAny(title, "\x00\r\n") {
		return "", fmt.Errorf("amp title %q contains a line break or NUL byte", title)
	}
	return title, nil
}

// ampRenameCommand builds the bounded rename call that re-applies a run
// title once the thread exists: amp may retitle the thread itself while
// the agent works, so `--title` alone doesn't guarantee the sidebar keeps
// the task's name. Best-effort by design — callers ignore a rename error
// once the run itself succeeded.
func ampRenameCommand(ctx context.Context, envFile, threadID, title string) (*exec.Cmd, error) {
	return ampRunCommand(ctx, envFile, []string{"threads", "rename", threadID, title})
}

// renameAmpThread is the rename var: production calls it through
// RenameAmpThread, tests swap it through AmpSwapForTest.
var renameAmpThread = renameAmpThreadImpl

// RenameAmpThread re-applies title to threadID and ignores every failure:
// the run already succeeded, and a rename must never turn that into a
// refusal. That includes archived threads, which amp refuses to rename —
// they keep whatever title they had.
func RenameAmpThread(ctx context.Context, envFile, threadID, title string) {
	if title == "" {
		return
	}
	renameAmpThread(ctx, envFile, threadID, title)
}

func renameAmpThreadImpl(ctx context.Context, envFile, threadID, title string) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd, err := ampRenameCommand(ctx, envFile, threadID, title)
	if err != nil {
		return
	}
	_ = cmd.Run()
}

// CheckAmpMode fails fast when the CLI rejects mode, quoting amp's own
// error: it runs the real run argv with a harmless prompt (`-x "ok"` last)
// before anything is spawned detached. --help and --version don't parse
// -m (both exit 0 for a bogus mode, confirmed live), while -x validates
// it before doing anything else — a bogus mode exits 1 in seconds
// (confirmed live ~8s). The probe costs one cheap turn that answers "ok".
func CheckAmpMode(ctx context.Context, envFile, mode string) error {
	return checkAmpMode(ctx, envFile, mode)
}

func checkAmpModeImpl(ctx context.Context, envFile, mode string) error {
	if mode == "" {
		return nil
	}
	if len(mode) > maxAmpModeBytes {
		return fmt.Errorf("amp mode %q is %d bytes (limit %d)", mode, len(mode), maxAmpModeBytes)
	}
	if strings.ContainsAny(mode, "\x00\r\n") {
		return fmt.Errorf("amp mode %q contains a line break or NUL byte", mode)
	}
	cmd, err := ampRunCommand(ctx, envFile, AmpRunArgs("ok", mode, "", ""))
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.Join(strings.Fields(string(out)), " ")
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("amp rejected mode %q: %s", mode, msg)
	}
	return nil
}
