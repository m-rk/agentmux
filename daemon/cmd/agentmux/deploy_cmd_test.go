package main

import (
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
)

func TestDeployTargetsDedupesAndAddsLocal(t *testing.T) {
	hosts := []hostsconfig.Host{
		{Name: "local", Gateway: "http://100.64.0.1:4288"},
		{Name: "LOCAL", Address: "unix:///run/agentmux/agentmuxd.sock"},
		{Name: "build-box", Gateway: "http://100.64.0.2:4288"},
	}
	targets := deployTargets(hosts)
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want 2 (local once, build-box once)", targets)
	}
	if targets[0].gateway != "http://100.64.0.1:4288" {
		t.Errorf("local target = %+v, want the gateway entry kept", targets[0])
	}
	if targets[1].name != "build-box" || targets[1].gateway == "" {
		t.Errorf("remote target = %+v", targets[1])
	}
}

func TestDeployTargetsAddsUnlistedLocal(t *testing.T) {
	hosts := []hostsconfig.Host{{Name: "build-box", Gateway: "http://100.64.0.2:4288"}}
	targets := deployTargets(hosts)
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want remote plus local", targets)
	}
	if targets[1].gateway != "" {
		t.Errorf("local target = %+v, want no gateway", targets[1])
	}
}

func TestDeployOwnedServicesNeverTouchInstances(t *testing.T) {
	for _, s := range deployOwnedServices() {
		if s.unit == "" || s.label == "" {
			t.Errorf("service = %+v, want unit and label", s)
		}
		if len(s.unit) > len("agentmux-") && s.unit[:len("agentmux-")] == "agentmux-" {
			rest := s.unit[len("agentmux-"):]
			if rest != "gateway.service" && rest != "asks.service" && rest != "threadwatch.service" {
				t.Errorf("service %q is not a known owned service", s.unit)
			}
		} else if s.unit != "agentmuxd.service" {
			t.Errorf("service %q is outside the agentmux namespace", s.unit)
		}
	}
}
