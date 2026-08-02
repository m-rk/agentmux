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

// runViewCmd is `agentmux view -instance NAME`: a headless, read-only
// snapshot of an instance's tmux pane — what you'd see if you attached and
// looked, without opening an interactive (and thus TTY-requiring) Attach
// session. Exists so a script or an agent driving another agentmux
// instance can check on a session's state without reaching around agentmux
// for raw tmux commands (which requires knowing the daemon's internal
// socket-naming convention).
func runViewCmd(args []string) {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "local", "device the instance lives on (a name from hosts.yaml, or \"local\")")
	instance := fs.String("instance", "", "instance name (required)")
	lines := fs.Int("lines", 0, "trailing lines of scrollback to include ahead of the visible pane; 0 = visible pane only")
	fs.Parse(args)

	if *instance == "" {
		log.Fatal("view: -instance is required")
	}

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("view: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.ViewPane(ctx, &pb.ViewPaneRequest{Instance: *instance, ScrollbackLines: int32(*lines)})
	if err != nil {
		log.Fatalf("view: %v", err)
	}
	fmt.Print(resp.Content)
}
