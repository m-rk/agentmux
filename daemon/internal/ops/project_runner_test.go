package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

func TestAddRunnerCodexDryRunAndIdempotent(t *testing.T) {
	c := newCreateEnv(t, "AGENTMUX_AGENT=claude-code", "AGENTMUX_PROJECT=sample/project", "AGENTMUX_RUN_USER=alice", "AGENTMUX_CODEX_ADD_DIRS=/shared")
	c.d.instances = []*pb.Instance{{Name: "tmpl", Agent: "claude-code", Workdir: c.repo}}
	req := RunnerAddRequest{From: "tmpl@" + address.LocalHostName(), Agent: "codex", DryRun: true}
	res, err := c.env.AddRunner(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Name != "app-codex" || len(res.Plan) == 0 || len(c.d.created) != 0 {
		t.Fatalf("dry run = %+v, created=%d", res, len(c.d.created))
	}
	req.DryRun = false
	// The fake registry must carry the source's run user; real provisioning
	// copies that field from the source instance.
	if err := os.WriteFile(filepath.Join(discovery.EnvDir, "tmpl.env"), []byte("AGENTMUX_AGENT=claude-code\nAGENTMUX_WORKDIR="+c.repo+"\nAGENTMUX_PROJECT=sample/project\nAGENTMUX_RUN_USER=alice\nAGENTMUX_CODEX_ADD_DIRS=/shared\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err = c.env.AddRunner(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || len(c.d.created) != 1 {
		t.Fatalf("create = %+v, calls=%d", res, len(c.d.created))
	}
	fields, err := session.ReadRegistry("app-codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fields["AGENTMUX_CODEX_ADD_DIRS"], "/shared") {
		t.Errorf("add dirs = %q", fields["AGENTMUX_CODEX_ADD_DIRS"])
	}
	res, err = c.env.AddRunner(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.Name != "app-codex" || len(c.d.created) != 1 {
		t.Fatalf("repeat = %+v, calls=%d", res, len(c.d.created))
	}
}
