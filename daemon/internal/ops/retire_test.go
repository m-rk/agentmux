package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// TestRetireDryRunNeedsNoDaemon is the privilege-split contract: the dry
// run touches nothing, so it must not dial the daemon — a dead dial is
// fine, and the failure names the missing instance, not the dial.
func TestRetireDryRunNeedsNoDaemon(t *testing.T) {
	env := Env{Dial: func() (Daemon, error) {
		return nil, errors.New("daemon should not be dialed for a dry run")
	}}
	addr := "task-nope@" + address.LocalHostName()
	_, err := env.Retire(context.Background(), RetireRequest{Address: addr, DryRun: true})
	if err == nil {
		t.Fatal("dry run of an unknown instance: nil error, want not_found")
	}
	if got := AsError(err).Reason; got != safesend.ReasonNotFound {
		t.Fatalf("reason = %s (%v), want not_found: the dial must not have been hit", got, err)
	}
}

// TestRetireDialsDaemonForManagedHalf: a real retire needs the daemon
// for stop/units/registry, so a dead dial fails before anything else.
func TestRetireDialsDaemonForManagedHalf(t *testing.T) {
	env := Env{Dial: func() (Daemon, error) {
		return nil, errors.New("no daemon here")
	}}
	addr := "task-nope@" + address.LocalHostName()
	_, err := env.Retire(context.Background(), RetireRequest{Address: addr})
	if err == nil || !strings.Contains(err.Error(), "no daemon here") {
		t.Fatalf("err = %v, want the dial failure", err)
	}
}

// TestRetireRefusesThreadAddress keeps the session-not-thread guard.
func TestRetireRefusesThreadAddress(t *testing.T) {
	env := Env{Dial: func() (Daemon, error) {
		return nil, errors.New("must not dial: the address is rejected first")
	}}
	addr := "task-1#T-00000000-0000-4000-8000-000000000001@" + address.LocalHostName()
	_, err := env.Retire(context.Background(), RetireRequest{Address: addr, DryRun: true})
	if got := AsError(err).Reason; got != safesend.ReasonInvalid {
		t.Fatalf("reason = %s (%v), want invalid", got, err)
	}
}
