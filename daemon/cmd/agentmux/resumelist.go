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

// runResumeListCmd is the `agentmux resume-list` subcommand entrypoint: the
// standalone, scriptable counterpart to ListResumableSessions, which
// otherwise is only reachable indirectly through the wizard's resume
// picker. Useful on its own for inspecting what a given workdir has to
// resume before deciding whether/what to pass to `agentmux new -resume`.
func runResumeListCmd(args []string) {
	fs := flag.NewFlagSet("resume-list", flag.ExitOnError)
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on (used when no hosts.yaml is found)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts to connect to")
	host := fs.String("host", "local", "device to look on (a name from hosts.yaml, or \"local\")")
	workdir := fs.String("workdir", "", "workdir to list resumable sessions for (required)")
	runUser := fs.String("run-user", "", "Linux only")
	fs.Parse(args)

	if *workdir == "" {
		log.Fatal("resume-list: -workdir is required")
	}

	client, err := dialOneHost(*hostsPath, *socketPath, *host)
	if err != nil {
		log.Fatalf("resume-list: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.ListResumableSessions(ctx, &pb.ListResumableSessionsRequest{Workdir: *workdir, RunUser: *runUser})
	if err != nil {
		log.Fatalf("resume-list: %v", err)
	}
	if len(resp.Sessions) == 0 {
		fmt.Println("no resumable sessions found")
		return
	}
	for _, s := range resp.Sessions {
		fmt.Printf("%s  %s\n", s.SessionId, relativeTime(s.LastModifiedUnix))
	}
}
