package main

import (
	"os"
	"path"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
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
	for _, s := range deployOwnedUserServices() {
		if s.unit == "" || s.label == "" {
			t.Errorf("user service = %+v, want unit and label", s)
		}
		if s.unit != "agentmux-asks-serve.service" {
			t.Errorf("user service %q is not a known owned user service", s.unit)
		}
	}
}

func TestDeployRunUserPrefersSudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "mark")
	if got := deployRunUser(); got != "mark" {
		t.Errorf("deployRunUser = %q, want mark", got)
	}
}

func TestDeployRunUserNeverRoot(t *testing.T) {
	os.Unsetenv("SUDO_USER")
	if got := deployRunUser(); got == "root" {
		t.Errorf("deployRunUser = root, want never root")
	}
}

func TestDeployHostsPathExplicitFlagWins(t *testing.T) {
	t.Setenv("SUDO_USER", "somebody")
	got, source := deployHostsPath("/tmp/custom-hosts.yaml")
	if got != "/tmp/custom-hosts.yaml" || source != "flag -hosts" {
		t.Errorf("deployHostsPath = %q (%s), want /tmp/custom-hosts.yaml (flag -hosts)", got, source)
	}
}

func TestDefaultSmokeNameFitsTaskGrants(t *testing.T) {
	if defaultSmokeName != "task-smoke-deploy" {
		t.Errorf("defaultSmokeName = %q, want task-smoke-deploy", defaultSmokeName)
	}
	// The fleet's create grants look like task-*@<host> (docs/gateway.md):
	// the default smoke name must match that pattern, or deploy's
	// dry-run create is refused as forbidden (AMUX-26).
	for _, pattern := range []string{"task-*@*", "task-*@laptop"} {
		ok, err := path.Match(pattern, defaultSmokeName+"@laptop")
		if err != nil || !ok {
			t.Errorf("path.Match(%q, %q) = %v, %v; want a match", pattern, defaultSmokeName+"@laptop", ok, err)
		}
	}
}

func TestSmokeCreateSkippableOnlyForbidden(t *testing.T) {
	for _, reason := range []safesend.Reason{safesend.ReasonForbidden} {
		if !smokeCreateSkippable(ops.Refuse(reason, "nope")) {
			t.Errorf("reason %q: want skippable", reason)
		}
	}
	for _, reason := range []safesend.Reason{
		safesend.ReasonFailed, safesend.ReasonNotFound, safesend.ReasonInvalid,
		safesend.ReasonNotLocal, safesend.ReasonRateLimited,
	} {
		if smokeCreateSkippable(ops.Refuse(reason, "nope")) {
			t.Errorf("reason %q: want failing, not skippable", reason)
		}
	}
}
