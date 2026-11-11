package ops

import (
	"context"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// claudeRelaunchWait bounds how long a run waits for a relaunched claude
// session to reach its input prompt before the prompt is delivered.
const claudeRelaunchWait = 90 * time.Second

// claudeDeliver is the live half of a claude run, replaceable in tests:
// relaunch the instance when relaunch is set (so it re-reads the pin from
// its registry), then paste the prompt into the session.
var claudeDeliver = func(ctx context.Context, e Env, instance, text string, relaunch bool) error {
	wait := 2 * time.Second
	if relaunch {
		c, err := e.dial()
		if err != nil {
			return err
		}
		resp, err := c.Control(ctx, instance, pb.ControlAction_CONTROL_RESTART)
		c.Close()
		if err != nil {
			return err
		}
		if !resp.Ok {
			return Refuse(safesend.ReasonFailed, "restarting %s: %s", instance, resp.Message)
		}
		wait = claudeRelaunchWait
	}
	var res SendResult
	return e.sendTmux(ctx, instance, "claude-code", text, wait, 5*time.Second, false, &res)
}

// runClaude is Run for a claude-code instance. A claude session is a
// resident process whose model, effort and speed are fixed by how it was
// launched, so a requested model, effort or speed that differs from the
// instance's current pin is recorded in its registry (the same
// AGENTMUX_CLAUDE_MODEL/EFFORT `sessions create` writes) and the session is
// relaunched, which passes `claude --model/--effort`; one that already
// matches, or none at all, leaves the running session alone. The prompt is
// then pasted into the session. An unknown model or effort is refused
// before anything changes. Claude has no thread addressing, so a thread
// suffix is refused.
func (e Env) runClaude(ctx context.Context, req RunRequest, addr address.Address, text string, fields map[string]string) (RunResult, error) {
	if t := strings.TrimSpace(addr.Thread); t != "" {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "claude-code has no thread addressing: drop %q", t)
	}
	if strings.TrimSpace(req.Sandbox) != "" {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "-sandbox is for codex instances only; %s runs claude-code", addr.Instance)
	}
	model, err := provision.CleanClaudeModel(req.Model)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	effort, err := provision.CleanClaudeEffort(req.Effort)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	speed, err := provision.CleanClaudeSpeed(req.Speed)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	curModel, curEffort, curSpeed := fields[provision.ClaudeModelKey], fields[provision.ClaudeEffortKey], fields[provision.ClaudeSpeedKey]
	newModel, newEffort, newSpeed := curModel, curEffort, curSpeed
	if model != "" {
		newModel = model
	}
	if effort != "" {
		newEffort = effort
	}
	if speed != "" {
		newSpeed = speed
	}
	relaunch := newModel != curModel || newEffort != curEffort || newSpeed != curSpeed

	if req.DryRun {
		step := "send prompt to claude-code session on " + addr.Session().String()
		if relaunch {
			step = "relaunch claude with model " + orDefault(newModel) + ", effort " + orDefault(newEffort) + ", speed " + orDefault(newSpeed) + ", then " + step
		}
		return RunResult{OK: true, Address: req.Address, Agent: "claude-code", DryRun: true, Plan: []string{step}}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, runTimeout+claudeRelaunchWait)
	defer cancel()
	if relaunch {
		if err := session.SetRegistryField(addr.Instance, provision.ClaudeModelKey, newModel); err != nil {
			return RunResult{}, Refuse(safesend.ReasonFailed, "recording model for %s: %v", addr.Instance, err)
		}
		if err := session.SetRegistryField(addr.Instance, provision.ClaudeEffortKey, newEffort); err != nil {
			_ = session.SetRegistryField(addr.Instance, provision.ClaudeModelKey, curModel)
			return RunResult{}, Refuse(safesend.ReasonFailed, "recording effort for %s: %v", addr.Instance, err)
		}
		if err := session.SetRegistryField(addr.Instance, provision.ClaudeSpeedKey, newSpeed); err != nil {
			_ = session.SetRegistryField(addr.Instance, provision.ClaudeModelKey, curModel)
			_ = session.SetRegistryField(addr.Instance, provision.ClaudeEffortKey, curEffort)
			return RunResult{}, Refuse(safesend.ReasonFailed, "recording speed for %s: %v", addr.Instance, err)
		}
	}
	if err := claudeDeliver(ctx, e, addr.Instance, text, relaunch); err != nil {
		if relaunch {
			// Put the old pin back so a later restart does not silently
			// pick up a pin whose run never happened.
			_ = session.SetRegistryField(addr.Instance, provision.ClaudeModelKey, curModel)
			_ = session.SetRegistryField(addr.Instance, provision.ClaudeEffortKey, curEffort)
			_ = session.SetRegistryField(addr.Instance, provision.ClaudeSpeedKey, curSpeed)
		}
		return RunResult{}, err
	}
	return RunResult{OK: true, Address: req.Address, Agent: "claude-code", State: "running"}, nil
}
