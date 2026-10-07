package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// codexLiveTimeout bounds the opt-in live proof's wait for the turn.
const codexLiveTimeout = 5 * time.Minute

// deployCodexTemplate is the first codex instance on a host, or "" when it
// has none (the codex smoke is then skipped, not failed).
func deployCodexTemplate(ctx context.Context, socketPath string, t deployTarget, local string) string {
	sessions, err := deployHostSessions(ctx, socketPath, t, local)
	if err != nil {
		return ""
	}
	for _, s := range sessions {
		if s.Agent == "codex" {
			return s.Name
		}
	}
	return ""
}

// deploySmokeCodex is the codex half of the deploy smoke test: a dry-run
// `sessions run` of the smoke name validated against the host's first codex
// instance, which starts no codex process and spends nothing. A host with no
// codex instance, or a gateway that refuses the run as forbidden, is
// skipped. With live set (-codex-live), the local host also runs one tiny
// read-only, low-effort turn on that instance and waits for it to finish —
// the only path that uses model quota.
func deploySmokeCodex(ctx context.Context, socketPath string, t deployTarget, local, smokeName string, live bool) error {
	tmpl := deployCodexTemplate(ctx, socketPath, t, local)
	if tmpl == "" {
		fmt.Printf("deploy: smoke %-12s codex skipped (no codex instance)\n", t.name)
		return nil
	}
	req := ops.RunRequest{
		Address:  smokeName + "@" + t.name,
		Template: tmpl,
		Text:     "deploy smoke test: reply with exactly: ok",
		DryRun:   true,
	}
	res, err := deployRunOn(ctx, socketPath, t, local, req)
	if err != nil {
		e := ops.AsError(err)
		if e.Reason == safesend.ReasonForbidden {
			fmt.Printf("deploy: smoke %-12s codex skipped (no grant for a run of smoke name %s: %s)\n", t.name, smokeName, e.Detail)
			return nil
		}
		return fmt.Errorf("smoke %s: codex dry-run run of %s: %s: %s", t.name, req.Address, e.Reason, e.Detail)
	}
	fmt.Printf("deploy: smoke %-12s codex dry run ok (%s)\n", t.name, strings.Join(res.Plan, "; "))
	if !live {
		return nil
	}
	if t.name != local {
		fmt.Printf("deploy: smoke %-12s codex live proof skipped (local host only)\n", t.name)
		return nil
	}
	return deployCodexLive(ctx, socketPath, t, tmpl)
}

// deployCodexLive runs one tiny read-only turn on a codex instance and
// waits for it to end done.
func deployCodexLive(ctx context.Context, socketPath string, t deployTarget, instance string) error {
	env := ops.Env{SocketPath: socketPath}
	run, err := env.Run(ctx, ops.RunRequest{
		Address: instance + "@" + t.name,
		Text:    "deploy smoke test: reply with exactly the word ok",
		Effort:  "low",
		Sandbox: "read-only",
	})
	if err != nil {
		e := ops.AsError(err)
		return fmt.Errorf("smoke %s: codex live run on %s: %s: %s", t.name, instance, e.Reason, e.Detail)
	}
	ctx, cancel := context.WithTimeout(ctx, codexLiveTimeout)
	defer cancel()
	for {
		st, err := env.Status(ctx, run.Address)
		if err == nil && st.Run != nil {
			switch st.Run.State {
			case "done":
				fmt.Printf("deploy: smoke %-12s codex live proof ok (%s)\n", t.name, run.Address)
				return nil
			case "failed":
				return fmt.Errorf("smoke %s: codex live run %s failed: %s", t.name, run.Address, st.Run.Reason)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("smoke %s: codex live run %s did not finish within %s", t.name, run.Address, codexLiveTimeout)
		case <-time.After(2 * time.Second):
		}
	}
}
