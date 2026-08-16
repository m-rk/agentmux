package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

// runControlCmd is `agentmux control -instance NAME -action start|stop|restart`:
// the scriptable counterpart to the TUI's s/x/r keybindings, which otherwise
// require an attached terminal to drive. Wraps the same Control RPC the
// daemon already exposes (and that RenameInstance's display-name path
// already drives internally) — previously the only headless way to restart
// a non-claude-code instance (e.g. to pick up a changed AGENTMUX_MODEL) was
// to bypass agentmux entirely and run `systemctl restart agentmux-<name>`
// by hand, which requires knowing the unit name and shell access to the
// host running the daemon.
func runControlCmd(args []string) {
	fs := flag.NewFlagSet("control", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "local", "device the instance lives on (a name from hosts.yaml, or \"local\")")
	instance := fs.String("instance", "", "instance name (required)")
	action := fs.String("action", "", "start|stop|restart (required)")
	force := fs.Bool("force", false, "allow targeting the instance this process is currently running inside of")
	fs.Parse(args)

	if *instance == "" {
		log.Fatal("control: -instance is required")
	}

	var pbAction pb.ControlAction
	switch *action {
	case "start":
		pbAction = pb.ControlAction_CONTROL_START
	case "stop":
		pbAction = pb.ControlAction_CONTROL_STOP
	case "restart":
		pbAction = pb.ControlAction_CONTROL_RESTART
	default:
		log.Fatalf("control: -action must be one of start|stop|restart, got %q", *action)
	}
	if *action != "start" {
		if err := refuseIfSelfTarget(*host, *instance, *force); err != nil {
			log.Fatalf("control: %v", err)
		}
	}

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("control: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Control(ctx, *instance, pbAction)
	if err != nil {
		log.Fatalf("control: %v", err)
	}
	if !resp.Ok {
		log.Fatalf("control: %s", resp.Message)
	}
	fmt.Println(resp.Message)
}
