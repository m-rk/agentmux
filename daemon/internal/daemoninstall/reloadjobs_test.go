package daemoninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
)

// jobPlist is an agentmux LaunchAgent whose first argument is prog.
func jobPlist(label, prog string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + label + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + prog + `</string>
        <string>session</string>
    </array>
</dict>
</plist>
`
}

const agentmuxBin = "/opt/agentmux/bin/agentmux"

func printOut(prog string) string {
	return "gui/501/x = {\n\tstate = running\n\tprogram = " + prog + "\n}\n"
}

type fakeJobs struct {
	loaded map[string]string // label -> loaded program
	calls  []string
}

func (f *fakeJobs) install(t *testing.T) {
	old := launchctl
	launchctl = func(args ...string) (string, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if args[0] == "print" {
			if prog, ok := f.loaded[strings.TrimPrefix(args[1], "gui/501/")]; ok {
				return printOut(prog), nil
			}
			return "not found", os.ErrNotExist
		}
		return "", nil
	}
	t.Cleanup(func() { launchctl = old })
}

func (f *fakeJobs) reregistered(label string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, "bootstrap ") && strings.HasSuffix(c, "/"+label+".plist") {
			return true
		}
	}
	return false
}

// The first install after AMUX-143 moves every agentmux job that still
// starts the binary directly onto the launcher, re-registering the loaded
// ones once; jobs already on the launcher, jobs that are not agentmux binary
// jobs, stopped jobs and the job running us are not re-registered.
func TestMigrateAgentJobs(t *testing.T) {
	dir := t.TempDir()
	launched := launchagent.Args(agentmuxBin, "session")
	files := map[string]string{
		"com.agentmux.alpha.plist":            jobPlist("com.agentmux.alpha", agentmuxBin),
		"com.agentmux.alpha.update.plist":     jobPlist("com.agentmux.alpha.update", agentmuxBin),
		"com.agentmux.stopped.plist":          jobPlist("com.agentmux.stopped", agentmuxBin),
		"com.agentmux.script.plist":           jobPlist("com.agentmux.script", "/opt/backend/run.sh"),
		"com.m-rk.agentmux.self-update.plist": jobPlist("com.m-rk.agentmux.self-update", agentmuxBin),
		"com.m-rk.agentmux.gateway.plist":     jobPlist("com.m-rk.agentmux.gateway", agentmuxBin),
		"com.example.other.plist":             jobPlist("com.example.other", agentmuxBin),
		"com.agentmux.notaplist.txt":          "",
	}
	wrappedBeta, _ := launchagent.Wrap([]byte(jobPlist("com.agentmux.beta", agentmuxBin)))
	files["com.agentmux.beta.plist"] = string(wrappedBeta)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeJobs{loaded: map[string]string{
		"com.agentmux.alpha": agentmuxBin, "com.agentmux.alpha.update": agentmuxBin,
		"com.agentmux.beta":             launched[0],
		"com.agentmux.script":           "/opt/backend/run.sh",
		"com.m-rk.agentmux.self-update": agentmuxBin, "com.m-rk.agentmux.gateway": agentmuxBin,
		"com.example.other": agentmuxBin,
	}}
	f.install(t)
	t.Setenv("XPC_SERVICE_NAME", "com.m-rk.agentmux.self-update")

	reloaded, err := migrateAgentJobs("gui/501", dir, map[string]bool{"com.m-rk.agentmux.gateway": true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(reloaded, ","), "com.agentmux.alpha,com.agentmux.alpha.update"; got != want {
		t.Errorf("reloaded = %s, want %s", got, want)
	}
	for _, label := range []string{"com.agentmux.beta", "com.agentmux.script", "com.agentmux.stopped", "com.m-rk.agentmux.self-update", "com.m-rk.agentmux.gateway", "com.example.other"} {
		if f.reregistered(label) {
			t.Errorf("%s must not be re-registered", label)
		}
	}
	for name, wantLaunched := range map[string]bool{
		"com.agentmux.alpha.plist": true, "com.agentmux.stopped.plist": true,
		"com.m-rk.agentmux.self-update.plist": true, // rewritten; its own run re-registers it
		"com.agentmux.beta.plist":             true,
		"com.agentmux.script.plist":           false, "com.m-rk.agentmux.gateway.plist": false, "com.example.other.plist": false,
	} {
		got, _ := os.ReadFile(filepath.Join(dir, name))
		if launchagent.Launched(launchagent.ProgramArguments(got)) != wantLaunched {
			t.Errorf("%s launched = %v, want %v", name, !wantLaunched, wantLaunched)
		}
	}
}

// Once every job is on the launcher, an update re-registers nothing: this is
// what stops the stack of macOS background-activity notifications (AMUX-143).
func TestMigrateAgentJobsSecondRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	for _, label := range []string{"com.agentmux.a", "com.agentmux.a.update", "com.m-rk.agentmux.self-update"} {
		if err := os.WriteFile(filepath.Join(dir, label+".plist"), []byte(jobPlist(label, agentmuxBin)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeJobs{loaded: map[string]string{"com.agentmux.a": agentmuxBin, "com.agentmux.a.update": agentmuxBin, "com.m-rk.agentmux.self-update": agentmuxBin}}
	f.install(t)
	t.Setenv("XPC_SERVICE_NAME", "")
	if _, err := migrateAgentJobs("gui/501", dir, nil); err != nil {
		t.Fatal(err)
	}
	for label := range f.loaded {
		f.loaded[label] = launchagent.Shell // what launchd now runs
	}
	f.calls = nil
	reloaded, err := migrateAgentJobs("gui/501", dir, nil)
	if err != nil || len(reloaded) != 0 {
		t.Fatalf("second run = %v, %v; want nothing", reloaded, err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "print ") {
			t.Errorf("second run called launchctl %s", c)
		}
	}
}

func TestMigrateAgentJobsReportsFailuresAndContinues(t *testing.T) {
	dir := t.TempDir()
	for _, label := range []string{"com.agentmux.a", "com.agentmux.b"} {
		if err := os.WriteFile(filepath.Join(dir, label+".plist"), []byte(jobPlist(label, agentmuxBin)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := launchctl
	launchctl = func(args ...string) (string, error) {
		if args[0] == "bootstrap" && strings.HasSuffix(args[2], "com.agentmux.a.plist") {
			return "Bootstrap failed: 5", os.ErrInvalid
		}
		return printOut(agentmuxBin), nil
	}
	defer func() { launchctl = old }()
	t.Setenv("XPC_SERVICE_NAME", "")
	reloaded, err := migrateAgentJobs("gui/501", dir, nil)
	if err == nil || !strings.Contains(err.Error(), "com.agentmux.a") {
		t.Fatalf("err = %v, want one naming com.agentmux.a", err)
	}
	if len(reloaded) != 1 || reloaded[0] != "com.agentmux.b" {
		t.Errorf("reloaded = %v, want [com.agentmux.b]", reloaded)
	}
}

func TestMigrateAgentJobsNoLaunchAgentsDir(t *testing.T) {
	if r, err := migrateAgentJobs("gui/501", filepath.Join(t.TempDir(), "none"), nil); err != nil || len(r) != 0 {
		t.Errorf("missing dir = %v, %v; want nothing, nil", r, err)
	}
}

func TestRefreshGatewayAgent(t *testing.T) {
	const gw = "com.m-rk.agentmux.gateway"
	launchedGW, _ := launchagent.Wrap([]byte(jobPlist(gw, agentmuxBin)))
	cases := []struct {
		name, onDisk string
		loaded       map[string]string
		want         string // the launchctl verb that restarts it
	}{
		{"on the launcher: kick only", string(launchedGW), map[string]string{gw: launchagent.Shell}, "kickstart"},
		{"still direct: migrate", jobPlist(gw, agentmuxBin), map[string]string{gw: agentmuxBin}, "bootstrap"},
		{"not loaded: load", string(launchedGW), map[string]string{}, "bootstrap"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), gw+".plist")
			if err := os.WriteFile(plist, []byte(c.onDisk), 0o644); err != nil {
				t.Fatal(err)
			}
			f := &fakeJobs{loaded: c.loaded}
			f.install(t)
			if err := refreshGatewayAgent("gui/501", plist); err != nil {
				t.Fatal(err)
			}
			calls := strings.Join(f.calls, ";")
			if c.want == "kickstart" && (calls != "print gui/501/"+gw+";kickstart -k gui/501/"+gw) {
				t.Errorf("calls = %q", calls)
			}
			if c.want == "bootstrap" && !f.reregistered(gw) {
				t.Errorf("calls = %q, want a bootstrap", calls)
			}
			got, _ := os.ReadFile(plist)
			if !launchagent.Launched(launchagent.ProgramArguments(got)) {
				t.Error("gateway plist is not on the launcher")
			}
		})
	}
	f := &fakeJobs{}
	f.install(t)
	if err := refreshGatewayAgent("gui/501", filepath.Join(t.TempDir(), "missing.plist")); err != nil || len(f.calls) != 0 {
		t.Errorf("missing gateway = %v, calls %q; want nothing", err, f.calls)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
