package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
)

func TestCheckHostsRecordsFailures(t *testing.T) {
	local := func(context.Context, string) (map[string]bool, error) { return map[string]bool{"codex": true}, nil }
	remote := func(_ context.Context, h string) (map[string]bool, error) {
		if h == "down" {
			return nil, errors.New("check failed: unreachable")
		}
		return map[string]bool{"amp": true}, nil
	}
	now := time.Unix(0, 0)
	got := checkHosts(context.Background(), []string{"local", "up", "down"}, now, local, remote)
	if len(got) != 3 {
		t.Fatalf("got %d reports", len(got))
	}
	if !got["local"].Runners["codex"] || !got["up"].Runners["amp"] {
		t.Errorf("reports: %+v", got)
	}
	if got["down"].Err != "check failed: unreachable" || got["down"].CheckedAt != now {
		t.Errorf("down: %+v", got["down"])
	}
}

func TestReadFuelReportsFailureOnTheTank(t *testing.T) {
	cat := catalogue.Catalogue{Fuel: []catalogue.FuelTank{{Name: "chatgpt", CapPercent: 50, CapSource: "manual"}, {Name: "claude"}}}
	readFuel(context.Background(), &cat, true, func(context.Context) (string, error) { return "", catalogue.ErrUnmasked })
	if cat.Fuel[0].Subscription == "" || cat.Fuel[1].Subscription != "" {
		t.Errorf("fuel: %+v", cat.Fuel)
	}
	readFuel(context.Background(), &cat, true, func(context.Context) (string, error) { return "present", nil })
	if cat.Fuel[0].Subscription != "present" || cat.Fuel[0].CapPercent != 50 {
		t.Errorf("fuel: %+v", cat.Fuel)
	}
	cat.Fuel[0].Subscription = ""
	readFuel(context.Background(), &cat, false, func(context.Context) (string, error) { return "present", nil })
	if cat.Fuel[0].Subscription != "" {
		t.Error("read without amp")
	}
}
