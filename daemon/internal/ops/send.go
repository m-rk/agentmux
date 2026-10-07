package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/retire"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// SendRequest is one message to deliver. By is the principal: the CLI takes
// it from -by; the gateway sets it from the caller's tailnet identity.
type SendRequest struct {
	Address     string
	Text        string
	Via         string
	By          string
	From        string
	Correlation string
	Wait        time.Duration // wait out a busy session for up to this long
	Confirm     time.Duration // watch this long for the turn starting
	// Doorbell makes the send a wake-up nudge: if the session is busy, or
	// already holds an undelivered doorbell, succeed without sending.
	Doorbell bool
}

// SendResult is the outcome, on success and on refusal.
type SendResult struct {
	OK          bool            `json:"ok"`
	Address     string          `json:"address"`
	Agent       string          `json:"agent,omitempty"`
	Thread      string          `json:"thread,omitempty"`
	Reason      safesend.Reason `json:"reason,omitempty"`
	Retryable   bool            `json:"retryable,omitempty"`
	Detail      string          `json:"detail,omitempty"`
	SubmittedAt *time.Time      `json:"submitted_at,omitempty"`
	// Confirmed is true when the session was seen to start a turn after the
	// submit (or an amp resume's run reports the worker running). False
	// means submitted but not observed, not that it failed.
	Confirmed bool `json:"confirmed"`
	// Queued is true when a codex send found the turn healthy and queued
	// the message for the thread's next turn instead of interrupting it.
	Queued bool `json:"queued,omitempty"`
	// Coalesced is true when a doorbell send succeeded without sending
	// because the session was busy or already had a doorbell waiting.
	Coalesced   bool   `json:"coalesced,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Correlation string `json:"correlation,omitempty"`
}

// sendAmpThread resolves which amp thread a send resumes. An explicit
// thread suffix wins (its shape is validated); otherwise the instance's
// current thread is used: the newest `sessions run` log first (found even
// when amp can't see the thread — archived, or the runner mapping never
// learned it), else the newest thread listed on the instance's runner.
// No thread anywhere is not_found with a hint at the equivalent sessions
// run.
func sendAmpThread(ctx context.Context, src transcript.Source, want string) (string, error) {
	if want != "" {
		if !transcript.ValidAmpThreadID(want) {
			return "", Refuse(safesend.ReasonInvalid, "%q is not an amp thread id", want)
		}
		return want, nil
	}
	threads, err := retire.LiveAmpThreads(ctx, src)
	if err != nil {
		// A listing failure (amp gone, CLI error) with no recorded
		// thread is still "nothing to resume": the run hint is what
		// the caller needs, not amp's stderr.
		return "", Refuse(safesend.ReasonNotFound, "no amp thread found for %s: run `agentmux sessions run -thread <T-id> -file <msg> %s@<host>` to continue the worker's own thread", src.Instance, src.Instance)
	}
	return threads[0], nil
}

// CleanText trims and validates message text.
func CleanText(raw string) (string, error) {
	text := strings.TrimRight(raw, "\n")
	if strings.TrimSpace(text) == "" {
		return "", errors.New("empty message")
	}
	if len(text) > safesend.MaxTextBytes {
		return "", fmt.Errorf("message is over %d bytes", safesend.MaxTextBytes)
	}
	if !utf8.ValidString(text) {
		return "", errors.New("message is not valid UTF-8")
	}
	// Control characters could end the bracketed paste early or drive the
	// TUI (an ESC sequence); only newlines and tabs are allowed.
	for _, r := range text {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return "", fmt.Errorf("message contains control character %U", r)
		}
	}
	return text, nil
}

// Send delivers one message to a local session with readiness checks, the
// provenance prefix and an audit entry. It never returns an error: the
// result says why it didn't send.
func (e Env) Send(ctx context.Context, req SendRequest) SendResult {
	res := SendResult{Address: req.Address, Correlation: req.Correlation}
	finish := func(err error) SendResult {
		if err != nil {
			r := AsError(err)
			res.OK, res.Reason, res.Retryable, res.Detail = false, r.Reason, r.Reason.Retryable(), r.Detail
		} else {
			res.OK = true
		}
		return res
	}

	addr, err := address.Parse(req.Address)
	if err != nil {
		return finish(Refuse(safesend.ReasonInvalid, "%v", err))
	}
	res.Thread = addr.Thread
	prov := safesend.Provenance{Via: req.Via, By: req.By, From: req.From}
	if err := prov.Validate(); err != nil {
		return finish(Refuse(safesend.ReasonInvalid, "%v", err))
	}
	if req.Correlation != "" && !safesend.ValidToken(req.Correlation) {
		return finish(Refuse(safesend.ReasonInvalid, "correlation %q: want 1-64 of letters, digits and ._:@/-", req.Correlation))
	}
	text, err := CleanText(req.Text)
	if err != nil {
		return finish(Refuse(safesend.ReasonInvalid, "%v", err))
	}
	message := prov.Compose(text)
	res.Bytes, res.SHA256 = safesend.Digest(message)

	if _, err := parseLocal(req.Address); err != nil {
		return finish(err)
	}
	src, _, err := Source(addr)
	if err != nil {
		return finish(err)
	}
	res.Agent = src.Agent

	// Every attempt past validation is audited; refuse to send at all if the
	// log can't be written.
	auditPath := safesend.AuditPath(src.Home)
	if err := checkAuditWritable(auditPath); err != nil {
		return finish(Refuse(safesend.ReasonFailed, "audit log %s not writable: %v", auditPath, err))
	}

	if src.Agent == "amp" {
		if req.Doorbell {
			err = Refuse(safesend.ReasonUnsupported, "-doorbell is not supported for amp instances: poll `sessions status` for the thread state instead")
		} else {
			err = sendAmpResume(ctx, src, addr, message, &res)
		}
	} else if src.Agent == "codex" {
		if req.Doorbell {
			err = Refuse(safesend.ReasonUnsupported, "-doorbell is not supported for codex instances: poll `sessions status` for the thread state instead")
		} else {
			err = sendCodexResume(ctx, src, addr, message, &res)
		}
	} else {
		err = e.sendTmux(ctx, addr.Instance, src.Agent, message, req.Wait, req.Confirm, req.Doorbell, &res)
	}
	// An amp send resumes the worker's thread through the run path; its
	// outcome says so, so the audit log never claims a terminal paste.
	outcome, detail := "delivered", ""
	if (src.Agent == "amp" || src.Agent == "codex") && err == nil {
		outcome = "resumed"
		if res.Queued {
			outcome = "queued"
		}
	}
	if err != nil {
		r := AsError(err)
		outcome, detail = string(r.Reason), r.Detail
	}
	if aerr := safesend.Append(auditPath, safesend.AuditEntry{
		Time: time.Now().UTC(), Principal: prov.By, Via: prov.Via, From: prov.From,
		Address: addr.Session().String(), Thread: res.Thread, Correlation: req.Correlation,
		Bytes: res.Bytes, SHA256: res.SHA256, Outcome: outcome, Detail: detail,
	}); aerr != nil {
		fmt.Fprintf(os.Stderr, "warning: writing audit log: %v\n", aerr)
	}
	return finish(err)
}

func checkAuditWritable(path string) error {
	if err := safesend.Append(path+".probe", safesend.AuditEntry{}); err != nil {
		return err
	}
	return os.Remove(path + ".probe")
}

// sendTmux delivers to a TUI agent through the local daemon: check the pane,
// paste as one message, submit, then watch for the turn starting.
func (e Env) sendTmux(ctx context.Context, instance, agent, message string, wait, confirm time.Duration, doorbell bool, res *SendResult) error {
	c, err := e.dial()
	if err != nil {
		return err
	}
	defer c.Close()

	instances, err := c.ListInstances(ctx)
	if err != nil {
		return err
	}
	live := false
	for _, inst := range instances {
		if inst.Name == instance {
			live = inst.Status != pb.Status_STATUS_DEAD && inst.TmuxSession != ""
		}
	}
	if !live {
		return Refuse(safesend.ReasonDead, "%s has no live session", instance)
	}

	deadline := time.Now().Add(wait)
ready:
	for {
		pane, err := c.ViewPane(ctx, &pb.ViewPaneRequest{Instance: instance, Escapes: true})
		if err != nil {
			return err
		}
		switch safesend.Classify(agent, pane.Content) {
		case safesend.StateReady:
			break ready
		case safesend.StateBusy:
			if doorbell {
				res.Coalesced = true
				return nil
			}
			if time.Now().Before(deadline) {
				time.Sleep(2 * time.Second)
				continue
			}
			return Refuse(safesend.ReasonBusy, "%s is mid-turn", instance)
		case safesend.StatePrompt:
			return Refuse(safesend.ReasonPrompt, "%s is showing a prompt that needs a person", instance)
		case safesend.StateDraft:
			if doorbell && safesend.PendingPaste(agent, pane.Content) {
				res.Coalesced = true
				return nil
			}
			return Refuse(safesend.ReasonDraft, "%s has unsent text in its input box", instance)
		default:
			return Refuse(safesend.ReasonUnsupported, "no readiness check for agent %q", agent)
		}
	}

	resp, err := c.SendText(ctx, &pb.SendTextRequest{Instance: instance, Text: message, Submit: true})
	if err != nil {
		return err
	}
	if !resp.Ok {
		return Refuse(safesend.ReasonFailed, "%s", resp.Message)
	}
	now := time.Now().UTC()
	res.SubmittedAt = &now

	until := time.Now().Add(confirm)
	for time.Now().Before(until) {
		time.Sleep(500 * time.Millisecond)
		pane, err := c.ViewPane(ctx, &pb.ViewPaneRequest{Instance: instance, Escapes: true})
		if err != nil {
			break
		}
		if safesend.Classify(agent, pane.Content) == safesend.StateBusy {
			res.Confirmed = true
			break
		}
	}
	return nil
}

// sendAmpResume delivers to an amp instance by resuming its thread through
// the `sessions run` path — never by pasting into the runner's terminal,
// which starts a new thread on amp's default model instead of the
// host-configured mode (and whose output nobody reads). The thread is the
// address's own suffix, else the instance's current thread (see
// sendAmpThread). The resume carries the provenance prefix as its text,
// exactly like a TUI send; the result's address and thread name the
// resumed thread, confirmed once the run reports the worker running.
//
// A send is always a nudge, so the resume sets InterruptStalled: when the
// thread's run is still working yet its stream log has gone quiet past
// session.AmpRunStalledAfter, the stuck local run child is stopped before
// the continue spawns, and the nudge starts a fresh turn instead of
// queuing behind one that never ends under a queue-default amp setting.
// A healthy running turn is never preempted — only a stalled one — and an
// explicit `sessions run -thread` continue never interrupts at all.
//
// The resumed run appends to the thread's own stream log, which is how
// status and read confirm the worker is running — nothing here pastes
// into the runner's terminal, so a send can never start an amp thread.
//
// The old contract waited for a thread-local runner to go idle (busy past
// -wait refused); resumes have no separate wait, they continue the thread
// the worker already owns.
func sendAmpResume(ctx context.Context, src transcript.Source, addr address.Address, message string, res *SendResult) error {
	thread, terr := sendAmpThread(ctx, src, addr.Thread)
	if terr != nil {
		return terr
	}
	runRes, rerr := Env{}.Run(ctx, RunRequest{Address: address.Address{Instance: addr.Instance, Host: addr.Host, Thread: thread}.String(), Text: message, InterruptStalled: true})
	if rerr != nil {
		return rerr
	}
	res.Address = runRes.Address
	res.Thread = runRes.Thread
	res.Confirmed = runRes.State == "running"
	now := time.Now().UTC()
	res.SubmittedAt = &now
	return nil
}
