package main

import (
	"os"
	"os/user"
	"path"
	"path/filepath"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
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

// TestSmokeRunSkippable pins the AMUX-27 rule: a forbidden run always
// skips (the fleet's run grants cover task-* sessions only), and nothing
// else does. A templated dry run validates against the template so the
// smoke target need not exist yet — not_found for the smoke name is
// gone — while a not_found for a missing template is a real failure.
func TestSmokeRunSkippable(t *testing.T) {
	if !smokeRunSkippable(ops.Refuse(safesend.ReasonForbidden, "nope")) {
		t.Error("forbidden run: want skippable")
	}
	for _, reason := range []safesend.Reason{
		safesend.ReasonFailed, safesend.ReasonNotFound, safesend.ReasonInvalid,
		safesend.ReasonNotLocal, safesend.ReasonRateLimited,
		safesend.ReasonUnsupported,
	} {
		if smokeRunSkippable(ops.Refuse(reason, "nope")) {
			t.Errorf("reason %q: want failing, not skippable", reason)
		}
	}
}

// TestDeployAmpConfigPathReadsSudoUser is the AMUX-48 regression test:
// under sudo, the smoke test's amp mode must come from the sudo user's
// config, not root's. A run-user home carrying amp.yaml resolves to
// that file even when the process itself runs as root.
func TestDeployAmpConfigPathReadsSudoUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	t.Setenv("SUDO_USER", me.Username)
	if got := deployRunUser(); got != me.Username {
		t.Fatalf("deployRunUser = %q, want %q", got, me.Username)
	}
	u, err := user.Lookup(me.Username)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(u.HomeDir, ".config", "agentmux", "amp.yaml")
	if got := deployAmpConfigPath(); got != want {
		t.Errorf("deployAmpConfigPath = %q, want %q", got, want)
	}
	if got := deployAmpConfigPath(); got == filepath.Join(string(filepath.Separator)+"root", ".config", "agentmux", "amp.yaml") {
		t.Errorf("deployAmpConfigPath points at root's config under sudo")
	}
}

// TestDeployLocalSmokeModePrefersInstanceOverride covers the mode order
// for the local smoke run: the template's AGENTMUX_AMP_MODE wins over
// the run user's amp.yaml, and an empty-everywhere resolves to "" so
// ops.Run's own Require refuses (never a default-model fallback).
func TestDeployLocalSmokeModePrefersInstanceOverride(t *testing.T) {
	envDir := t.TempDir()
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	if err := os.WriteFile(filepath.Join(envDir, "tmpl.env"),
		[]byte("AGENTMUX_AGENT=amp\nAGENTMUX_AMP_MODE=instance-mode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	t.Setenv("SUDO_USER", me.Username)
	got, merr := deployLocalSmokeMode("tmpl")
	if merr != nil {
		t.Fatalf("deployLocalSmokeMode: %v", merr)
	}
	if got != "instance-mode" {
		t.Errorf("deployLocalSmokeMode = %q, want instance-mode", got)
	}
}

// TestDeployAmpNamesKeepsOnlyAmp sorts the no-checkout skip: it names the
// amp instances whose workdirs were all outside checkouts, not the
// unrelated sessions around them.
func TestDeployAmpNamesKeepsOnlyAmp(t *testing.T) {
	sessions := []ops.Session{
		{Name: "web", Agent: "claude-code"},
		{Name: "a1", Agent: "amp"},
		{Name: "a2", Agent: "amp"},
	}
	got := deployAmpNames(sessions)
	if len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Errorf("deployAmpNames = %q, want [a1 a2]", got)
	}
	if len(deployAmpNames(nil)) != 0 {
		t.Error("deployAmpNames(nil) is not empty")
	}
}
