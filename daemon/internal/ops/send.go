package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/pb"
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
	// submit (or amp returned the thread). False means submitted but not
	// observed, not that it failed.
	Confirmed   bool   `json:"confirmed"`
	Bytes       int    `json:"bytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Correlation string `json:"correlation,omitempty"`
}

var ampThreadURL = regexp.MustCompile(`T-[0-9A-Fa-f-]{8,}`)

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
		err = sendAmp(ctx, src, addr, message, req.Wait, &res)
	} else {
		err = e.sendTmux(ctx, addr.Instance, src.Agent, message, req.Wait, req.Confirm, &res)
	}
	outcome, detail := "delivered", ""
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
func (e Env) sendTmux(ctx context.Context, instance, agent, message string, wait, confirm time.Duration, res *SendResult) error {
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
			if time.Now().Before(deadline) {
				time.Sleep(2 * time.Second)
				continue
			}
			return Refuse(safesend.ReasonBusy, "%s is mid-turn", instance)
		case safesend.StatePrompt:
			return Refuse(safesend.ReasonPrompt, "%s is showing a prompt that needs a person", instance)
		case safesend.StateDraft:
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

// sendAmp posts to a runner thread through the amp CLI, or starts a new
// thread on the instance's runner when the address names none.
func sendAmp(ctx context.Context, src transcript.Source, addr address.Address, message string, wait time.Duration, res *SendResult) error {
	if src.AmpRunnerID == "" {
		return Refuse(safesend.ReasonUnsupported, "%s has no amp runner id", addr.Instance)
	}
	var out []byte
	var err error
	if addr.Thread == "" {
		out, err = transcript.AmpRun(ctx, src, "--execute="+message, "--executor", "runner:"+src.AmpRunnerID)
	} else {
		if !transcript.ValidAmpThreadID(addr.Thread) {
			return Refuse(safesend.ReasonInvalid, "%q is not an amp thread id", addr.Thread)
		}
		deadline := time.Now().Add(wait)
		for {
			state, serr := transcript.AmpThreadState(ctx, src, addr.Thread)
			if errors.Is(serr, transcript.ErrNoThread) {
				return Refuse(safesend.ReasonNotFound, "thread %s is not on runner %s", addr.Thread, src.AmpRunnerID)
			}
			if serr != nil {
				return serr
			}
			if state == "" || state == "idle" {
				break
			}
			if time.Now().Before(deadline) {
				time.Sleep(5 * time.Second)
				continue
			}
			return Refuse(safesend.ReasonBusy, "thread %s is %s", addr.Thread, state)
		}
		out, err = transcript.AmpRun(ctx, src, "threads", "continue", addr.Thread, "--orb-execute", "--execute="+message)
	}
	if err != nil {
		return Refuse(safesend.ReasonFailed, "amp: %v", err)
	}
	now := time.Now().UTC()
	res.SubmittedAt = &now
	if id := ampThreadURL.FindString(string(out)); id != "" {
		res.Thread = id
		res.Confirmed = true
	}
	return nil
}
