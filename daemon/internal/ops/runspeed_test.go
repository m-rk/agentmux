package ops

import (
	"context"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// A speed pin on a claude-code run is recorded and relaunches the session;
// the same speed leaves it alone; a failed delivery puts the old one back.
func TestRunClaudeSpeed(t *testing.T) {
	cases := []struct {
		name         string
		extra        []string
		speed        string
		deliverErr   error
		wantRelaunch bool
		wantSpeed    string
		wantErr      bool
	}{
		{name: "fast relaunches", speed: "fast", wantRelaunch: true, wantSpeed: "fast"},
		{name: "same speed leaves it alone", extra: []string{"AGENTMUX_CLAUDE_SPEED=fast"}, speed: "fast", wantSpeed: "fast"},
		{name: "omitted keeps the pin", extra: []string{"AGENTMUX_CLAUDE_SPEED=fast"}, wantSpeed: "fast"},
		{name: "standard turns it off", extra: []string{"AGENTMUX_CLAUDE_SPEED=fast"}, speed: "standard", wantRelaunch: true, wantSpeed: "standard"},
		{name: "failed delivery restores", extra: []string{"AGENTMUX_CLAUDE_SPEED=standard"}, speed: "fast", deliverErr: Refuse(safesend.ReasonBusy, "mid-turn"), wantRelaunch: true, wantSpeed: "standard", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := newModelRunEnv(t, "claude-code", tc.extra...)
			var gotRelaunch bool
			old := claudeDeliver
			claudeDeliver = func(_ context.Context, _ Env, instance, _ string, relaunch bool) error {
				gotRelaunch = relaunch
				if fields, _ := session.ReadRegistry(instance); relaunch && tc.speed != "" && fields[provision.ClaudeSpeedKey] != tc.speed {
					t.Errorf("speed not recorded before relaunch: %v", fields)
				}
				return tc.deliverErr
			}
			t.Cleanup(func() { claudeDeliver = old })
			_, err := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi", Speed: tc.speed})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if gotRelaunch != tc.wantRelaunch {
				t.Fatalf("relaunch = %v, want %v", gotRelaunch, tc.wantRelaunch)
			}
			if fields, _ := session.ReadRegistry("inst"); fields[provision.ClaudeSpeedKey] != tc.wantSpeed {
				t.Fatalf("speed pin = %q, want %q", fields[provision.ClaudeSpeedKey], tc.wantSpeed)
			}
		})
	}
}

// An unknown speed is refused, and so is any speed for an agent that has no
// fast mode.
func TestRunSpeedRefusals(t *testing.T) {
	addr := newModelRunEnv(t, "claude-code")
	_, err := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi", DryRun: true, Speed: "turbo"})
	wantReason(t, err, safesend.ReasonInvalid)
	for _, agent := range []string{"codex", "opencode"} {
		addr := newModelRunEnv(t, agent)
		_, err := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi", DryRun: true, Speed: "fast"})
		wantReason(t, err, safesend.ReasonUnsupported)
	}
	addr = newModelRunEnv(t, "claude-code")
	res, err := Env{}.Run(context.Background(), RunRequest{Address: addr, Text: "hi", DryRun: true, Speed: "fast"})
	if err != nil || !strings.Contains(strings.Join(res.Plan, "\n"), "speed fast") {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
}
