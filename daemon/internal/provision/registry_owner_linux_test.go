//go:build linux

package provision

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// TestChownRegistryForUserChownsToTheGivenUser exercises the actual
// syscall (chowning to the current user, so it works without root) rather
// than just checking for a nil error, since a chown that silently no-ops
// would defeat the whole point of chownRegistryForUser.
func TestChownRegistryForUserChownsToTheGivenUser(t *testing.T) {
	dir := withEnvDir(t)
	path := filepath.Join(dir, "probe.env")
	if err := os.WriteFile(path, []byte("AGENTMUX_INSTANCE_NAME=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	self, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := chownRegistryForUser("probe", self); err != nil {
		t.Fatalf("chownRegistryForUser: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("could not read raw stat_t for uid check")
	}
	wantUID, err := strconv.Atoi(self.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Uid) != wantUID {
		t.Errorf("registry file uid = %d, want %d (%s)", st.Uid, wantUID, self.Username)
	}
}

func TestReadRegistryRunUser(t *testing.T) {
	dir := withEnvDir(t)
	path := filepath.Join(dir, "probe.env")
	content := "AGENTMUX_INSTANCE_NAME=probe\nAGENTMUX_RUN_USER=dev\nAGENTMUX_WORKDIR=/home/dev/.agentmux/probe\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := readRegistryRunUser(path), "dev"; got != want {
		t.Errorf("readRegistryRunUser = %q, want %q", got, want)
	}

	empty := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(empty, []byte("AGENTMUX_INSTANCE_NAME=empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readRegistryRunUser(empty); got != "" {
		t.Errorf("readRegistryRunUser with no AGENTMUX_RUN_USER field = %q, want \"\"", got)
	}

	if got := readRegistryRunUser(filepath.Join(dir, "missing.env")); got != "" {
		t.Errorf("readRegistryRunUser for a missing file = %q, want \"\"", got)
	}
}

// TestSelfHealRegistryOwnershipSkipsWhenNotRoot confirms the function is a
// clean no-op rather than an error when the calling process isn't root,
// matching what any normal test run (and most real deployments outside the
// daemon's own root-owned unit) actually is.
func TestSelfHealRegistryOwnershipSkipsWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test asserts the non-root no-op path; running as root")
	}
	dir := withEnvDir(t)
	path := filepath.Join(dir, "probe.env")
	if err := os.WriteFile(path, []byte("AGENTMUX_INSTANCE_NAME=probe\nAGENTMUX_RUN_USER=root\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	// Must not panic or attempt (and fail on) a chown it has no permission
	// for; SelfHealRegistryOwnership has no return value to assert on, so
	// this test's value is that it runs to completion at all.
	SelfHealRegistryOwnership()
}
