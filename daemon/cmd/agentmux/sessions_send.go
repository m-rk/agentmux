package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// runSessionsSend is `agentmux sessions send`: deliver one message to a local
// session with readiness checks, a provenance prefix and an audit entry. See
// docs/design/gateway.md (phase 3) and ops.Env.Send. Exit 0 delivered, 1
// refused or failed, 2 usage.
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
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 || (fs.NArg() == 2) == (*file != "") {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions send -by PRINCIPAL [-via relayed|dispatched|sent] [-from REF] [-correlation ID] [-hosts PATH] [-wait DUR] [-json] <instance>@<host>[#<thread>] (TEXT | -file PATH|-)")
		os.Exit(2)
	}

	req := ops.SendRequest{
		Address: fs.Arg(0), Via: *via, By: *by, From: *from,
		Correlation: *correlation, Wait: *wait, Confirm: *confirm,
	}
	var res ops.SendResult
	text, err := readSendText(fs.Arg(1), *file)
	route, rerr := resolveRoute(req.Address, *hostsPath, address.LocalHostName())
	switch {
	case err != nil:
		res = ops.SendResult{Address: req.Address, Reason: safesend.ReasonInvalid, Detail: err.Error(), Correlation: req.Correlation}
	case rerr != nil:
		e := ops.AsError(rerr)
		res = ops.SendResult{Address: req.Address, Reason: e.Reason, Retryable: e.Reason.Retryable(), Detail: e.Detail, Correlation: req.Correlation}
	case route.Remote != nil:
		req.Text = text
		if *by != "" {
			fmt.Fprintln(os.Stderr, "note: -by ignored for a remote session; the gateway sets the sender from your tailnet identity")
		}
		ctx, cancel := context.WithTimeout(context.Background(), *wait+*confirm+gatewayclient.SendMargin+time.Minute)
		defer cancel()
		res = route.Remote.Send(ctx, remoteSendRequest(req))
	default:
		req.Text = text
		ctx, cancel := context.WithTimeout(context.Background(), *wait+*confirm+3*time.Minute)
		defer cancel()
		res = ops.Env{SocketPath: *socketPath}.Send(ctx, req)
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

// remoteSendRequest maps a send to the gateway's wire form. By is dropped:
// the gateway sets the principal from the caller's tailnet identity.
func remoteSendRequest(req ops.SendRequest) gatewayapi.SendRequest {
	return gatewayapi.SendRequest{
		Address: req.Address, Text: req.Text, Via: req.Via, From: req.From,
		Correlation: req.Correlation,
		WaitSeconds: int(req.Wait / time.Second), ConfirmSeconds: int(req.Confirm / time.Second),
	}
}

func readSendText(arg, file string) (string, error) {
	switch {
	case file == "-":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, safesend.MaxTextBytes+1))
		return string(data), err
	case file != "":
		data, err := os.ReadFile(file)
		return string(data), err
	default:
		return arg, nil
	}
}
