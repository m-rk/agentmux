// Command agentmuxd is the per-host daemon: it reports agentmux instance
// status, streams PTY attach sessions, and drives lifecycle control, all
// over a gRPC service bound to a Unix socket (phase 1: localhost only).
package main

import (
	"flag"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"

	"github.com/m-rk/agentmux/daemon/internal/daemonserver"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func main() {
	socketPath := flag.String("socket", "/run/agentmux/agentmuxd.sock", "Unix socket to listen on")
	flag.Parse()

	if err := os.Remove(*socketPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("removing stale socket %s: %v", *socketPath, err)
	}

	lis, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listening on %s: %v", *socketPath, err)
	}
	if err := os.Chmod(*socketPath, 0o666); err != nil {
		log.Printf("warning: could not chmod %s: %v", *socketPath, err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterAgentmuxDaemonServer(grpcServer, daemonserver.New())

	log.Printf("agentmuxd listening on %s", *socketPath)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
