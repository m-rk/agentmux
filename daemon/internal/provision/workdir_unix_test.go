//go:build darwin || linux

package provision

import (
	"fmt"
	"os/user"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkdirMkdirCommandRunsAsTargetUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(u.HomeDir, "agentmux-workdir-test")

	cmd, err := workdirMkdirCommand(workdir, u)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"/bin/mkdir", "-p", "-m", "0755", "--", workdir}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", cmd.Args, wantArgs)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("mkdir command has no target-user credential")
	}
	if got, want := cmd.SysProcAttr.Credential.Uid, parseIDForTest(t, u.Uid); got != want {
		t.Fatalf("UID = %d, want %d", got, want)
	}
	if got, want := cmd.SysProcAttr.Credential.Gid, parseIDForTest(t, u.Gid); got != want {
		t.Fatalf("GID = %d, want %d", got, want)
	}
}

func TestWorkdirMkdirCommandRejectsRelativePath(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workdirMkdirCommand("relative/path", u); err == nil {
		t.Fatal("expected relative workdir to be rejected")
	}
}

func TestWorkdirMkdirCommandRejectsInvalidUserID(t *testing.T) {
	u := &user.User{Username: "broken", Uid: "not-a-uid", Gid: "1", HomeDir: "/tmp"}
	if _, err := workdirMkdirCommand("/tmp/agentmux", u); err == nil {
		t.Fatal("expected invalid UID to be rejected")
	}
}

func parseIDForTest(t *testing.T, value string) uint32 {
	t.Helper()
	var parsed uint32
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}
