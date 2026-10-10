package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// runCodex is Run for a codex instance: `codex exec --json` detached in the
// worktree (a new thread), or `codex exec resume <thread>` (a continue),
// with the prompt on stdin from a private file. It returns once codex
// prints thread.started; `sessions status` then reads the JSONL log.
func (e Env) runCodex(ctx context.Context, req RunRequest, addr address.Address, text string, fields map[string]string, workdir string) (RunResult, error) {
	thread := strings.TrimSpace(addr.Thread)
	if thread != "" && !session.ValidCodexThreadID(thread) {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%q is not a codex thread id", thread)
	}
	title, err := session.CleanAmpTitle(req.Title)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", strings.Replace(err.Error(), "amp title", "title", 1))
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = fields["AGENTMUX_MODEL"]
	}
	effort := strings.TrimSpace(req.Effort)
	if effort == "" {
		effort = fields["AGENTMUX_CODEX_EFFORT"]
	}
	sandbox := strings.TrimSpace(req.Sandbox)
	if sandbox == "" {
		sandbox = fields["AGENTMUX_CODEX_SANDBOX"]
	}
	argv, err := session.CodexRunArgs(session.CodexRunOptions{
		Workdir:     workdir,
		Model:       model,
		Effort:      effort,
		Sandbox:     sandbox,
		AddDirs:     session.CodexAddDirs(fields[session.CodexAddDirsKey]),
		AllowUnsafe: fields[session.CodexUnsafeSandboxEnv] == "1",
	}, thread)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	if req.DryRun {
		step := "start codex thread on " + addr.Session().String()
		if thread != "" {
			step = "continue codex thread " + addr.String()
		}
		if model != "" {
			step += " with model " + model
		}
		return RunResult{OK: true, Address: req.Address, Agent: "codex", DryRun: true, Plan: []string{step}}, nil
	}
	if thread == "" && title != "" {
		text = title + "\n\n" + text
	}
	src, _, err := Source(addr)
	if err != nil {
		return RunResult{}, err
	}
	if _, err := os.Stat(src.Home); err != nil {
		return RunResult{}, Refuse(safesend.ReasonFailed, "home %s is not readable: %v", src.Home, err)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	logPath := session.CodexRunLogPath(src.Home, addr.Instance, thread)
	if thread != "" {
		// Two `codex exec` processes on one thread would interleave the
		// rollout, so a continue refuses while the turn is still working —
		// unless a send found it stalled, in which case it is stopped first
		// (the amp InterruptStalled behaviour).
		if st := session.CodexRunStateOf(logPath); st.State == "running" && fileExists(logPath) {
			if !req.InterruptStalled || !st.Stalled {
				return RunResult{}, Refuse(safesend.ReasonBusy, "codex thread %s is still working; poll `sessions status` and continue when it is done", thread)
			}
			session.StopCodexRuns(addr.Instance, workdir)
		}
	}
	id, err := session.StartCodexRun(ctx, addr.Instance, argv, text, workdir, logPath)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonFailed, "codex: %v", err)
	}
	if thread == "" {
		// The log was pending until the thread id was known; move it under
		// the thread's own name so status finds it (codex keeps appending
		// to the same open file).
		_ = os.Rename(logPath, session.CodexRunLogPath(src.Home, addr.Instance, id))
	}
	full := address.Address{Instance: addr.Instance, Host: addr.Host, Thread: id}
	return RunResult{OK: true, Address: full.String(), Agent: "codex", Thread: id, ThreadID: id, State: "running"}, nil
}

// codexStatus fills a status result for a codex instance: with a thread
// address, the run state read from that thread's JSONL log; without one the
// instance is idle (codex has no resident process to classify).
func codexStatus(res StatusResult, addr address.Address) (StatusResult, error) {
	res.State = "idle"
	if addr.Thread == "" {
		return res, nil
	}
	if !session.ValidCodexThreadID(addr.Thread) {
		return res, Refuse(safesend.ReasonInvalid, "%q is not a codex thread id", addr.Thread)
	}
	src, _, err := Source(addr)
	if err != nil {
		return res, err
	}
	logPath := session.CodexRunLogPath(src.Home, addr.Instance, addr.Thread)
	if !fileExists(logPath) {
		return res, Refuse(safesend.ReasonNotFound, "thread %s has no run on this host", addr.Thread)
	}
	st := session.CodexRunStateOf(logPath)
	res.Run = &RunStateInfo{State: st.State, Reason: st.Reason, RateLimited: st.RateLimited, Stalled: st.Stalled}
	res.State = st.State
	return res, nil
}

// sendCodexResume delivers a send to a codex worker as a new turn: `codex
// exec resume` on the named thread, or the instance's newest run thread. A
// stalled turn is stopped first (as for amp); a live one refuses busy.
func sendCodexResume(ctx context.Context, src transcript.Source, addr address.Address, message string, res *SendResult) error {
	thread := addr.Thread
	if thread == "" {
		var err error
		if thread, err = newestCodexThread(src); err != nil {
			return err
		}
	}
	// A healthy turn is never killed: the message is queued on the thread
	// and codex delivers it with the thread's next turn. A stalled turn
	// falls through to Run, which stops it and starts a new one.
	if st := session.CodexRunStateOf(session.CodexRunLogPath(src.Home, addr.Instance, thread)); st.State == "running" && !st.Stalled {
		if err := session.QueueCodexMessage(ctx, addr.Instance, thread, message); err != nil {
			return Refuse(safesend.ReasonFailed, "%v", err)
		}
		res.Address = address.Address{Instance: addr.Instance, Host: addr.Host, Thread: thread}.String()
		res.Thread = thread
		res.Queued = true
		now := time.Now().UTC()
		res.SubmittedAt = &now
		return nil
	}
	runRes, err := Env{}.Run(ctx, RunRequest{Address: address.Address{Instance: addr.Instance, Host: addr.Host, Thread: thread}.String(), Text: message, InterruptStalled: true})
	if err != nil {
		return err
	}
	res.Address = runRes.Address
	res.Thread = runRes.Thread
	res.Confirmed = runRes.State == "running"
	now := time.Now().UTC()
	res.SubmittedAt = &now
	return nil
}

// newestCodexThread is the thread of the instance's most recently written
// run log, or not_found with the `sessions run` hint.
func newestCodexThread(src transcript.Source) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(session.CodexRunStateDir(src.Home, src.Instance), "codex-run-*.jsonl"))
	var best string
	var bestTime time.Time
	for _, m := range matches {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "codex-run-"), ".jsonl")
		info, err := os.Stat(m)
		if err != nil || !session.ValidCodexThreadID(id) {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = id, info.ModTime()
		}
	}
	if best == "" {
		return "", Refuse(safesend.ReasonNotFound, "no codex thread found for %s: run `agentmux sessions run -file <msg> %s@<host>` to start one", src.Instance, src.Instance)
	}
	return best, nil
}
