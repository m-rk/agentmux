package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

// runRemoveCmd is the local, headless `agentmux remove -instance NAME`.
func runRemoveCmd(args []string) {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	instance := fs.String("instance", "", "instance to stop, disable, and archive")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	_ = fs.Parse(args)
	if *instance == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux remove -instance NAME [-socket PATH]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := (ops.Env{SocketPath: *socketPath}).Remove(ctx, ops.RemoveRequest{Instance: *instance})
	if err != nil {
		e := ops.AsError(err)
		fmt.Fprintf(os.Stderr, "not removed %s: %s: %s\n", *instance, e.Reason, e.Detail)
		os.Exit(1)
	}
	fmt.Println(res.Message)
	fmt.Println("workdir and agent data left in place")
}
