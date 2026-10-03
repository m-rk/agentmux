package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
	"github.com/m-rk/agentmux/daemon/internal/tuiclient"
)

// sendResult is `sessions send -json` output, on success and on refusal.
type sendResult struct {
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

// errRefused carries a refusal reason out of the delivery helpers.
type errRefused struct {
	reason safesend.Reason
	detail string
}

func (e errRefused) Error() string { return string(e.reason) + ": " + e.detail }

func refuse(r safesend.Reason, format string, a ...any) error {
	return errRefused{reason: r, detail: fmt.Sprintf(format, a...)}
}

var ampThreadURL = regexp.MustCompile(`T-[0-9A-Fa-f-]{8,}`)

// runSessionsSend is `agentmux sessions send`: deliver one message to a local
// session with readiness checks, a provenance prefix and an audit entry. See
// docs/design/gateway.md (phase 3). Exit 0 delivered, 1 refused or failed,
// 2 usage.
func runSessionsSend(args []string) {
	fs := flag.NewFlagSet("sessions send", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON (also on refusal)")
	via := fs.String("via", "relayed", "provenance verb: "+strings.Join(safesend.Vias, ", "))
	by := fs.String("by", "", "sending principal for the provenance prefix and audit log (required)")
	from := fs.String("from", "", "optional provenance source, e.g. a task id")
	correlation := fs.String("correlation", "", "opaque id recorded in the audit log, not in the message")
	file := fs.String("file", "", "read the message from this file (\"-\" for stdin) instead of the argument")
	wait := fs.Duration("wait", 0, "if the session is busy, wait up to this long for it to finish before refusing")
	confirm := fs.Duration("confirm", 15*time.Second, "how long to watch for the session starting a turn")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 || (fs.NArg() == 2) == (*file != "") {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions send -by PRINCIPAL [-via relayed|dispatched|sent] [-from REF] [-correlation ID] [-wait DUR] [-json] <instance>@<host>[#<thread>] (TEXT | -file PATH|-)")
		os.Exit(2)
	}

	res := sendResult{Address: fs.Arg(0), Correlation: *correlation}
	finish := func(err error) {
		if err != nil {
			var r errRefused
			if !errors.As(err, &r) {
				r = errRefused{reason: safesend.ReasonFailed, detail: err.Error()}
			}
			res.OK, res.Reason, res.Retryable, res.Detail = false, r.reason, r.reason.Retryable(), r.detail
		} else {
			res.OK = true
		}
		if *jsonOut {
			writeJSON(res)
		} else if res.OK {
			state := "submitted; turn start not observed"
			if res.Confirmed {
				state = "confirmed"
			}
			fmt.Printf("delivered to %s (%s)\n", res.Address, state)
		} else {
			fmt.Fprintf(os.Stderr, "not sent to %s: %s: %s\n", res.Address, res.Reason, res.Detail)
		}
		if !res.OK {
			os.Exit(1)
		}
	}

	addr, err := address.Parse(fs.Arg(0))
	if err != nil {
		finish(refuse(safesend.ReasonInvalid, "%v", err))
	}
	res.Thread = addr.Thread
	prov := safesend.Provenance{Via: *via, By: *by, From: *from}
	if err := prov.Validate(); err != nil {
		finish(refuse(safesend.ReasonInvalid, "%v", err))
	}
	if *correlation != "" && !safesend.ValidToken(*correlation) {
		finish(refuse(safesend.ReasonInvalid, "correlation %q: want 1-64 of letters, digits and ._:@/-", *correlation))
	}
	text, err := readSendText(fs, *file)
	if err != nil {
		finish(refuse(safesend.ReasonInvalid, "%v", err))
	}
	message := prov.Compose(text)
	res.Bytes, res.SHA256 = safesend.Digest(message)

	src, _, err := localTranscriptSource(addr)
	if err != nil {
		if addr.Host != address.LocalHostName() {
			finish(refuse(safesend.ReasonNotLocal, "%v", err))
		}
		finish(refuse(safesend.ReasonNotFound, "%v", err))
	}
	res.Agent = src.Agent

	// Every attempt past validation is audited; refuse to send at all if the
	// log can't be written.
	auditPath := safesend.AuditPath(src.Home)
	audit := func(outcome, detail string) error {
		return safesend.Append(auditPath, safesend.AuditEntry{
			Time: time.Now().UTC(), Principal: prov.By, Via: prov.Via, From: prov.From,
			Address: addr.Session().String(), Thread: res.Thread, Correlation: *correlation,
			Bytes: res.Bytes, SHA256: res.SHA256, Outcome: outcome, Detail: detail,
		})
	}
	if err := checkAuditWritable(auditPath); err != nil {
		finish(refuse(safesend.ReasonFailed, "audit log %s not writable: %v", auditPath, err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait+*confirm+3*time.Minute)
	defer cancel()
	if src.Agent == "amp" {
		err = sendAmp(ctx, src, addr, message, *wait, &res)
	} else {
		err = sendTmux(ctx, *socketPath, addr.Instance, src.Agent, message, *wait, *confirm, &res)
	}
	outcome, detail := "delivered", ""
	if err != nil {
		var r errRefused
		if errors.As(err, &r) {
			outcome, detail = string(r.reason), r.detail
		} else {
			outcome, detail = string(safesend.ReasonFailed), err.Error()
		}
	}
	if aerr := audit(outcome, detail); aerr != nil {
		fmt.Fprintf(os.Stderr, "warning: writing audit log: %v\n", aerr)
	}
	finish(err)
}

func readSendText(fs *flag.FlagSet, file string) (string, error) {
	var data []byte
	var err error
	switch {
	case file == "-":
		data, err = io.ReadAll(io.LimitReader(os.Stdin, safesend.MaxTextBytes+1))
	case file != "":
		data, err = os.ReadFile(file)
	default:
		data = []byte(fs.Arg(1))
	}
	if err != nil {
		return "", err
	}
	text := strings.TrimRight(string(data), "\n")
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

func checkAuditWritable(path string) error {
	if err := safesend.Append(path+".probe", safesend.AuditEntry{}); err != nil {
		return err
	}
	return os.Remove(path + ".probe")
}

// sendTmux delivers to a TUI agent through the local daemon: check the pane,
// paste as one message, submit, then watch for the turn starting.
func sendTmux(ctx context.Context, socketPath, instance, agent, message string, wait, confirm time.Duration, res *sendResult) error {
	c, err := tuiclient.Dial("local", "unix://"+socketPath)
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
		return refuse(safesend.ReasonDead, "%s has no live session", instance)
	}

	deadline := time.Now().Add(wait)
	for {
		pane, err := c.ViewPane(ctx, &pb.ViewPaneRequest{Instance: instance})
		if err != nil {
			return err
		}
		switch safesend.Classify(agent, pane.Content) {
		case safesend.StateReady:
			goto deliver
		case safesend.StateBusy:
			if time.Now().Before(deadline) {
				time.Sleep(2 * time.Second)
				continue
			}
			return refuse(safesend.ReasonBusy, "%s is mid-turn", instance)
		case safesend.StatePrompt:
			return refuse(safesend.ReasonPrompt, "%s is showing a prompt that needs a person", instance)
		case safesend.StateDraft:
			return refuse(safesend.ReasonDraft, "%s has unsent text in its input box", instance)
		default:
			return refuse(safesend.ReasonUnsupported, "no readiness check for agent %q", agent)
		}
	}

deliver:
	resp, err := c.SendText(ctx, &pb.SendTextRequest{Instance: instance, Text: message, Submit: true})
	if err != nil {
		return err
	}
	if !resp.Ok {
		return refuse(safesend.ReasonFailed, "%s", resp.Message)
	}
	now := time.Now().UTC()
	res.SubmittedAt = &now

	until := time.Now().Add(confirm)
	for time.Now().Before(until) {
		time.Sleep(500 * time.Millisecond)
		pane, err := c.ViewPane(ctx, &pb.ViewPaneRequest{Instance: instance})
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
func sendAmp(ctx context.Context, src transcript.Source, addr address.Address, message string, wait time.Duration, res *sendResult) error {
	if src.AmpRunnerID == "" {
		return refuse(safesend.ReasonUnsupported, "%s has no amp runner id", addr.Instance)
	}
	var out []byte
	var err error
	if addr.Thread == "" {
		out, err = transcript.AmpRun(ctx, src, "--execute="+message, "--executor", "runner:"+src.AmpRunnerID)
	} else {
		if !transcript.ValidAmpThreadID(addr.Thread) {
			return refuse(safesend.ReasonInvalid, "%q is not an amp thread id", addr.Thread)
		}
		deadline := time.Now().Add(wait)
		for {
			state, err := transcript.AmpThreadState(ctx, src, addr.Thread)
			if errors.Is(err, transcript.ErrNoThread) {
				return refuse(safesend.ReasonNotFound, "thread %s is not on runner %s", addr.Thread, src.AmpRunnerID)
			}
			if err != nil {
				return err
			}
			if state == "" || state == "idle" {
				break
			}
			if time.Now().Before(deadline) {
				time.Sleep(5 * time.Second)
				continue
			}
			return refuse(safesend.ReasonBusy, "thread %s is %s", addr.Thread, state)
		}
		out, err = transcript.AmpRun(ctx, src, "threads", "continue", addr.Thread, "--orb-execute", "--execute="+message)
	}
	if err != nil {
		return refuse(safesend.ReasonFailed, "amp: %v", err)
	}
	now := time.Now().UTC()
	res.SubmittedAt = &now
	if id := ampThreadURL.FindString(string(out)); id != "" {
		res.Thread = id
		res.Confirmed = true
	}
	return nil
}
