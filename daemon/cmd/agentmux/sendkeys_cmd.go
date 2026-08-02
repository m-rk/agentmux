package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

// runSendKeysCmd is `agentmux send-keys -instance NAME KEY...`: a headless
// equivalent of typing into an instance's pane, without opening an
// interactive Attach session. Trailing positional arguments are passed
// through verbatim as tmux send-keys arguments — literal text and/or key
// names ("Escape", "Enter", "C-c", ...), exactly like `tmux send-keys`
// itself — so e.g. `agentmux send-keys -instance minecraft Escape`
// dismisses a stuck confirmation menu, and
// `agentmux send-keys -instance minecraft '/remote-control' Enter` sends a
// slash command.
//
// This is scriptable access to the same capability Attach already grants
// interactively (typing into the target session), not a new one — see
// SendKeys's proto doc comment.
func runSendKeysCmd(args []string) {
	fs := flag.NewFlagSet("send-keys", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "local", "device the instance lives on (a name from hosts.yaml, or \"local\")")
	instance := fs.String("instance", "", "instance name (required)")
	fs.Parse(args)

	if *instance == "" {
		log.Fatal("send-keys: -instance is required")
	}
	keys := fs.Args()
	if len(keys) == 0 {
		log.Fatal("send-keys: at least one key/text argument is required, e.g. `agentmux send-keys -instance NAME Escape`")
	}

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("send-keys: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.SendKeys(ctx, &pb.SendKeysRequest{Instance: *instance, Keys: keys})
	if err != nil {
		log.Fatalf("send-keys: %v", err)
	}
	if !resp.Ok {
		log.Fatalf("send-keys: %s", resp.Message)
	}
}
