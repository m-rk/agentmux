package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/daemonserver"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
)

func runDaemonCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux daemon <install|uninstall|status|run>")
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		if err := daemoninstall.Install(); err != nil {
			log.Fatalf("daemon install: %v", err)
		}
	case "uninstall":
		if err := daemoninstall.Uninstall(); err != nil {
			log.Fatalf("daemon uninstall: %v", err)
		}
	case "status":
		status, err := daemoninstall.Status()
		if err != nil {
			log.Fatalf("daemon status: %v", err)
		}
		fmt.Println(status)
	case "run":
		runDaemon(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown daemon subcommand %q\n", args[0])
		os.Exit(1)
	}
}

// runDaemon is `agentmux daemon run`: the foreground daemon process
// (formerly the standalone agentmuxd binary). This is what the unit
// installed by `agentmux daemon install` execs.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon run", flag.ExitOnError)
	socketPath := fs.String("socket", "/run/agentmux/agentmuxd.sock", "Unix socket to listen on")
	listenAddr := fs.String("listen", "", "TCP address to also listen on, e.g. the host's Tailscale IP:port (disabled if empty). No TLS/auth is applied here; restrict access via tailnet ACLs.")
	envDir := fs.String("env-dir", discovery.EnvDir, "directory to read instance *.env files from (override for testing without root)")
	fs.Parse(args)
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
