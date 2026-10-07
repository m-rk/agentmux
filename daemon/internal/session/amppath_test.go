package session

import (
	"reflect"
	"strings"
	"testing"
)

func swapHostLoginPath(p string) func() {
	old := hostLoginPath
	hostLoginPath = func() string { return p }
	return func() { hostLoginPath = old }
}

func TestAmpWorkerPathPutsStubThenLoginPathWithoutDuplicates(t *testing.T) {
	defer swapHostLoginPath("/opt/go/bin:/usr/bin:/home/alice/.cargo/bin")()
	t.Setenv("PATH", "/usr/bin:/bin")
	got := AmpWorkerPath("/home/alice/.agentmux/stubs")
	want := "/home/alice/.agentmux/stubs:/opt/go/bin:/usr/bin:/home/alice/.cargo/bin:/bin"
	if got != want {
		t.Fatalf("AmpWorkerPath = %q, want %q", got, want)
	}
}

func TestAmpWorkerPathFallsBackToProcessPath(t *testing.T) {
	defer swapHostLoginPath("")()
	t.Setenv("PATH", "/usr/bin:/bin")
	if got := AmpWorkerPath(""); got != "/usr/bin:/bin" {
		t.Fatalf("AmpWorkerPath = %q", got)
	}
}

// TestCheckAmpToolchainRecordsEnvironment uses a fake runner that records
// the environment each probe was given and fails rg.
func TestCheckAmpToolchainRecordsEnvironment(t *testing.T) {
	defer swapHostLoginPath("/opt/go/bin")()
	t.Setenv("PATH", "/usr/bin")
	old := ampToolRun
	defer func() { ampToolRun = old }()
	var envs [][]string
	var names []string
	ampToolRun = func(env []string, name string, args ...string) error {
		envs = append(envs, env)
		names = append(names, name)
		if strings.HasSuffix(name, "rg") {
			return errFakeMissing
		}
		return nil
	}
	missing := CheckAmpToolchain()
	if !reflect.DeepEqual(missing, []string{"rg"}) {
		t.Fatalf("missing = %v, want [rg]", missing)
	}
	if len(envs) != 2 {
		t.Fatalf("ran %d probes (%v), want 2", len(envs), names)
	}
	for _, env := range envs {
		found := ""
		for _, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				found = kv
			}
		}
		if found != "PATH=/opt/go/bin:/usr/bin" {
			t.Errorf("probe PATH = %q, want the login PATH", found)
		}
	}
	w := AmpToolchainWarning("task-1", missing)
	if w == "" || strings.Contains(w, "\n") || !strings.Contains(w, "rg") {
		t.Errorf("warning = %q, want one line naming rg", w)
	}
	if AmpToolchainWarning("task-1", nil) != "" {
		t.Error("no warning expected when nothing is missing")
	}
}

type fakeMissing struct{}

func (fakeMissing) Error() string { return "not found" }

var errFakeMissing error = fakeMissing{}
