// Command agentmuxd is the per-host daemon: it reports agentmux instance
// status, streams PTY attach sessions, and drives lifecycle control, all
// over a gRPC service. It always binds a Unix socket for local use, and
// optionally a TCP address (phase 2: a host's Tailscale IP) for remote
// TUI clients.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"

	"github.com/m-rk/agentmux/daemon/internal/daemonserver"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func main() {
	socketPath := flag.String("socket", "/run/agentmux/agentmuxd.sock", "Unix socket to listen on")
	listenAddr := flag.String("listen", "", "TCP address to also listen on, e.g. the host's Tailscale IP:port (disabled if empty). No TLS/auth is applied here; restrict access via tailnet ACLs.")
	envDir := flag.String("env-dir", discovery.EnvDir, "directory to read instance *.env files from (override for testing without root)")
	flag.Parse()
	discovery.EnvDir = *envDir

	if err := os.Remove(*socketPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("removing stale socket %s: %v", *socketPath, err)
	}

	unixLis, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listening on %s: %v", *socketPath, err)
	}
	if err := os.Chmod(*socketPath, 0o666); err != nil {
		log.Printf("warning: could not chmod %s: %v", *socketPath, err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterAgentmuxDaemonServer(grpcServer, daemonserver.New())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("agentmuxd listening on %s", *socketPath)
		if err := grpcServer.Serve(unixLis); err != nil {
			log.Fatalf("serve %s: %v", *socketPath, err)
		}
	}()

	if *listenAddr != "" {
		tcpLis, err := net.Listen("tcp", *listenAddr)
		if err != nil {
			log.Fatalf("listening on %s: %v", *listenAddr, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("agentmuxd listening on %s (tcp)", *listenAddr)
			if err := grpcServer.Serve(tcpLis); err != nil {
				log.Fatalf("serve %s: %v", *listenAddr, err)
			}
		}()
	}

	wg.Wait()
}
