package daemonserver

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/taskcheck"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/test/bufconn"
)

func TestRunTaskCheckUsesOnlyRegistryTargetAndReturnsResult(t *testing.T) {
	envDir := t.TempDir()
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	if err := os.WriteFile(filepath.Join(envDir, "task-76.env"), []byte(
		"AGENTMUX_INSTANCE_NAME=task-76\nAGENTMUX_AGENT=codex\nAGENTMUX_PROJECT=m-rk/agentmux\nAGENTMUX_WORKDIR=/work/task-76\nAGENTMUX_RUN_USER=worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	s := &Server{runTaskCheck: func(ctx context.Context, target taskcheck.Target) taskcheck.Result {
		called = true
		if target.Instance != "task-76" || target.Project != "m-rk/agentmux" || target.Workdir != "/work/task-76" || target.RunUser != "worker" {
			t.Errorf("runner target = %+v", target)
		}
		return taskcheck.Result{Project: target.Project, Category: "test_failure", Output: "go test output", ExitCode: 1, Duration: 1500 * time.Millisecond}
	}}
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterAgentmuxDaemonServer(grpcServer, s)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///taskcheck-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	resp, err := pb.NewAgentmuxDaemonClient(conn).RunTaskCheck(ctx, &pb.RunTaskCheckRequest{Instance: "task-76"})
	if err != nil || !resp.Ok || !called {
		t.Fatalf("RunTaskCheck = %+v, called=%v, err=%v", resp, called, err)
	}
	if resp.Project != "m-rk/agentmux" || resp.Result != "test_failure" || resp.Output != "go test output" || resp.ExitCode != 1 || resp.DurationMs != 1500 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestRunTaskCheckRefusesNonCodexAndUntrustedProject(t *testing.T) {
	envDir := t.TempDir()
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	for _, tc := range []struct{ instance, values string }{
		{"service", "AGENTMUX_INSTANCE_NAME=service\nAGENTMUX_AGENT=codex\nAGENTMUX_PROJECT=m-rk/agentmux\nAGENTMUX_WORKDIR=/work\nAGENTMUX_RUN_USER=worker\n"},
		{"task-77", "AGENTMUX_INSTANCE_NAME=task-77\nAGENTMUX_AGENT=claude-code\nAGENTMUX_PROJECT=m-rk/agentmux\nAGENTMUX_WORKDIR=/work\nAGENTMUX_RUN_USER=worker\n"},
		{"task-78", "AGENTMUX_INSTANCE_NAME=task-78\nAGENTMUX_AGENT=codex\nAGENTMUX_PROJECT=other/project\nAGENTMUX_WORKDIR=/work\nAGENTMUX_RUN_USER=worker\n"},
		{"task-79", "AGENTMUX_INSTANCE_NAME=task-79\nAGENTMUX_AGENT=codex\nAGENTMUX_PROJECT=m-rk/agentmux\nAGENTMUX_WORKDIR=/work\nAGENTMUX_RUN_USER=worker\n"},
	} {
		if err := os.WriteFile(filepath.Join(envDir, tc.instance+".env"), []byte(tc.values), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	s := &Server{runTaskCheck: func(context.Context, taskcheck.Target) taskcheck.Result {
		called = true
		return taskcheck.Result{Category: "passed"}
	}}
	for _, instance := range []string{"service", "task-77", "task-78", "../task-78"} {
		resp, err := s.RunTaskCheck(context.Background(), &pb.RunTaskCheckRequest{Instance: instance})
		if err != nil || resp.Ok || resp.Message == "" {
			t.Errorf("RunTaskCheck(%s) = %+v, %v; want refusal", instance, resp, err)
		}
	}
	if called {
		t.Fatal("runner was called for a refused request")
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}})
	resp, err := s.RunTaskCheck(ctx, &pb.RunTaskCheckRequest{Instance: "task-79"})
	if err != nil || resp.Ok || resp.Message == "" {
		t.Fatalf("remote task check = %+v, %v; want local-socket refusal", resp, err)
	}
}
