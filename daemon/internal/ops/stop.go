package ops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// StopRequest cancels the active run of one instance. By, Via and From are
// the principal, as for a send: the CLI takes By from -by, the gateway sets
// it from the caller's tailnet identity.
type StopRequest struct {
	Address     string // <instance>@<host>[#<thread>]
	Via         string
	By          string
	From        string
	Correlation string
	// DryRun reports what would be stopped and stops nothing.
	DryRun bool
}

// StopResult is the outcome of a stop that was not refused.
type StopResult struct {
	OK      bool   `json:"ok"`
	Address string `json:"address"`
	Agent   string `json:"agent,omitempty"`
	Thread  string `json:"thread,omitempty"`
	// State is "stopped" when a run was cancelled, "idle" when there was
	// none to cancel, or the finished run's own state ("done", "failed",
	// "stopped") when the thread was already over. A dry run says
	// "running" for a run it would stop.
	State string `json:"state"`
	// Stopped is true when this call cancelled a run.
	Stopped     bool       `json:"stopped"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	DryRun      bool       `json:"dry_run,omitempty"`
	Plan        []string   `json:"plan,omitempty"`
	Correlation string     `json:"correlation,omitempty"`
}

// claudeStopWait bounds how long a claude-code stop waits for the session to
// leave its turn after Escape.
const claudeStopWait = 10 * time.Second

// Stop cancels the active run of a local instance and leaves the instance,
// its worktree and its branch alone. A codex or amp run (the address's
// thread, else the instance's newest run thread) is ended through
// session.StopCodexRun / StopAmpRunThread, after which its run state reads
// stopped and `sessions run -thread` continues it like any finished thread.
// A claude-code session mid-turn gets Escape, Claude Code's own interrupt.
// Stopping an idle instance succeeds with State "idle". Every stop past
// validation is audited, like a send.
func (e Env) Stop(ctx context.Context, req StopRequest) (StopResult, error) {
	res := StopResult{Address: req.Address, Correlation: req.Correlation}
	addr, err := parseLocal(req.Address)
	if err != nil {
		return res, err
	}
	res.Thread = addr.Thread
	prov := safesend.Provenance{Via: req.Via, By: req.By, From: req.From}
	if err := prov.Validate(); err != nil {
		return res, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	if req.Correlation != "" && !safesend.ValidToken(req.Correlation) {
		return res, Refuse(safesend.ReasonInvalid, "correlation %q: want 1-64 of letters, digits and ._:@/-", req.Correlation)
	}
	src, _, err := Source(addr)
	if err != nil {
		return res, err
	}
	res.Agent = src.Agent
	auditPath := safesend.AuditPath(src.Home)
	if !req.DryRun {
		if err := checkAuditWritable(auditPath); err != nil {
			return res, Refuse(safesend.ReasonFailed, "audit log %s not writable: %v", auditPath, err)
		}
	}

	switch src.Agent {
	case "codex":
		err = stopCodex(ctx, src, addr, req, &res)
	case "amp":
		err = stopAmp(ctx, src, addr, req, &res)
	case "claude-code":
		err = e.stopClaude(ctx, addr, req, &res)
	default:
		err = Refuse(safesend.ReasonUnsupported, "stop is not supported for %s instances", src.Agent)
	}
	if req.DryRun {
		if err == nil {
			res.OK, res.DryRun = true, true
		}
		return res, err
	}
	outcome, detail := res.State, ""
	if err != nil {
		r := AsError(err)
		outcome, detail = string(r.Reason), r.Detail
	}
	if aerr := safesend.Append(auditPath, safesend.AuditEntry{
		Time: time.Now().UTC(), Principal: prov.By, Via: prov.Via, From: prov.From,
		Address: addr.Session().String(), Thread: res.Thread, Correlation: req.Correlation,
		Outcome: "stop:" + outcome, Detail: detail,
	}); aerr != nil {
		fmt.Fprintf(os.Stderr, "warning: writing audit log: %v\n", aerr)
	}
	if err != nil {
		return res, err
	}
	res.OK = true
	return res, nil
}

// stopThread is the thread a codex or amp stop acts on: the address's own,
// else the one of the instance's newest run log (prefix "codex-run-" or
// "amp-run-"); "" when the instance has never run.
func stopThread(src transcript.Source, addr address.Address, prefix string, valid func(string) bool) (string, error) {
	if t := strings.TrimSpace(addr.Thread); t != "" {
		if !valid(t) {
			return "", Refuse(safesend.ReasonInvalid, "%q is not a %s thread id", t, src.Agent)
		}
		return t, nil
	}
	// Codex and amp keep their run logs in the same per-instance directory.
	matches, _ := filepath.Glob(filepath.Join(session.CodexRunStateDir(src.Home, src.Instance), prefix+"*.jsonl"))
	var best string
	var bestTime time.Time
	for _, m := range matches {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), prefix), ".jsonl")
		info, err := os.Stat(m)
		if err != nil || !valid(id) {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = id, info.ModTime()
		}
	}
	return best, nil
}

func stopCodex(ctx context.Context, src transcript.Source, addr address.Address, req StopRequest, res *StopResult) error {
	thread, err := stopThread(src, addr, "codex-run-", session.ValidCodexThreadID)
	if err != nil {
		return err
	}
	res.Thread = thread
	if thread == "" {
		res.State = "idle"
		return nil
	}
	logPath := session.CodexRunLogPath(src.Home, addr.Instance, thread)
	if !fileExists(logPath) {
		if addr.Thread != "" {
			return Refuse(safesend.ReasonNotFound, "thread %s has no run on this host", thread)
		}
		res.State = "idle"
		return nil
	}
	st := session.CodexRunStateOf(logPath)
	if st.State != "running" {
		res.State = st.State
		return nil
	}
	if req.DryRun {
		res.State = "running"
		res.Plan = []string{"stop codex run on thread " + thread + " (SIGTERM, then SIGKILL after a grace period); the instance, worktree and branch stay"}
		return nil
	}
	st, stopped, err := session.StopCodexRun(ctx, addr.Instance, src.Workdir, logPath, req.By)
	res.State, res.Stopped = st.State, stopped
	if err != nil {
		return Refuse(safesend.ReasonFailed, "%v", err)
	}
	res.StoppedAt = parseStopTime(st.StoppedAt)
	return nil
}

func stopAmp(ctx context.Context, src transcript.Source, addr address.Address, req StopRequest, res *StopResult) error {
	thread, err := stopThread(src, addr, "amp-run-", transcript.ValidAmpThreadID)
	if err != nil {
		return err
	}
	res.Thread = thread
	if thread == "" {
		res.State = "idle"
		return nil
	}
	logPath := session.AmpRunLogPath(src.Home, addr.Instance, thread)
	if !fileExists(logPath) {
		if addr.Thread != "" {
			return Refuse(safesend.ReasonNotFound, "thread %s has no run on this host", thread)
		}
		res.State = "idle"
		return nil
	}
	st := session.AmpRunStateOf(logPath)
	if st.State != "running" && st.State != "waiting" {
		res.State = st.State
		return nil
	}
	if req.DryRun {
		res.State = st.State
		res.Plan = []string{"stop amp run on thread " + thread + "; the instance, worktree and branch stay"}
		return nil
	}
	st, stopped, err := session.StopAmpRunThread(ctx, addr.Instance, src.Workdir, logPath, req.By)
	res.State, res.Stopped = st.State, stopped
	if err != nil {
		return Refuse(safesend.ReasonFailed, "%v", err)
	}
	res.StoppedAt = parseStopTime(st.StoppedAt)
	return nil
}

// claudeStopKeys sends the interrupt key to a claude-code session;
// replaceable in tests.
var claudeStopKeys = func(ctx context.Context, e Env, instance string) error {
	c, err := e.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	resp, err := c.SendKeys(ctx, &pb.SendKeysRequest{Instance: instance, Keys: []string{"Escape"}})
	if err != nil {
		return err
	}
	if !resp.Ok {
		return Refuse(safesend.ReasonFailed, "%s", resp.Message)
	}
	return nil
}

// claudePaneState classifies a claude-code pane; replaceable in tests.
var claudePaneState = func(ctx context.Context, e Env, instance string) (safesend.State, error) {
	c, err := e.dial()
	if err != nil {
		return safesend.StateUnknown, err
	}
	defer c.Close()
	instances, err := c.ListInstances(ctx)
	if err != nil {
		return safesend.StateUnknown, err
	}
	live := false
	for _, inst := range instances {
		if inst.Name == instance {
			live = inst.Status != pb.Status_STATUS_DEAD && inst.TmuxSession != ""
		}
	}
	if !live {
		return safesend.StateUnknown, Refuse(safesend.ReasonDead, "%s has no live session", instance)
	}
	pane, err := c.ViewPane(ctx, &pb.ViewPaneRequest{Instance: instance, Escapes: true})
	if err != nil {
		return safesend.StateUnknown, err
	}
	return safesend.Classify("claude-code", pane.Content), nil
}

// stopClaude interrupts a claude-code turn with Escape. Only a busy pane is
// touched: Escape on a prompt or a draft would answer or clear something a
// person may be looking at.
func (e Env) stopClaude(ctx context.Context, addr address.Address, req StopRequest, res *StopResult) error {
	if t := strings.TrimSpace(addr.Thread); t != "" {
		return Refuse(safesend.ReasonInvalid, "claude-code has no thread addressing: drop %q", t)
	}
	state, err := claudePaneState(ctx, e, addr.Instance)
	if err != nil {
		return err
	}
	if state != safesend.StateBusy {
		res.State = "idle"
		return nil
	}
	if req.DryRun {
		res.State = "running"
		res.Plan = []string{"press Escape in the claude-code session to interrupt its turn"}
		return nil
	}
	if err := claudeStopKeys(ctx, e, addr.Instance); err != nil {
		return err
	}
	deadline := time.Now().Add(claudeStopWait)
	for {
		state, err = claudePaneState(ctx, e, addr.Instance)
		if err == nil && state != safesend.StateBusy {
			now := time.Now().UTC()
			res.State, res.Stopped, res.StoppedAt = "stopped", true, &now
			return nil
		}
		if time.Now().After(deadline) {
			return Refuse(safesend.ReasonFailed, "%s was still mid-turn %s after Escape", addr.Instance, claudeStopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func parseStopTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
