package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstanceEnv(t *testing.T, envDir, name, workdir string) {
	t.Helper()
	body := "AGENTMUX_WORKDIR=" + workdir + "\nAGENTMUX_TMUX_SESSION=" + name + "\n"
	if err := os.WriteFile(filepath.Join(envDir, name+".env"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckExistingWorkdirAcceptsFreeFolder(t *testing.T) {
	withEnvDir(t)
	dir := t.TempDir()
	if err := checkExistingWorkdir("notes", dir); err != nil {
		t.Fatalf("existing free folder refused: %v", err)
	}
}

func TestCheckExistingWorkdirRefusesBadPaths(t *testing.T) {
	withEnvDir(t)
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ path, want string }{
		"missing":  {filepath.Join(t.TempDir(), "nope"), "does not exist"},
		"file":     {file, "not a directory"},
		"relative": {"some/dir", "absolute"},
		"empty":    {"", "no workdir"},
	}
	for name, c := range cases {
		err := checkExistingWorkdir("notes", c.path)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
}

func TestCheckExistingWorkdirRefusesAnotherInstancesFolder(t *testing.T) {
	envDir := withEnvDir(t)
	dir := t.TempDir()
	writeInstanceEnv(t, envDir, "first", dir)

	err := checkExistingWorkdir("second", dir)
	if err == nil || !strings.Contains(err.Error(), `"first"`) {
		t.Fatalf("duplicate workdir: err = %v, want a refusal naming the other instance", err)
	}

	// A symlink to the same folder collides too.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := checkExistingWorkdir("second", link); err == nil {
		t.Fatal("symlinked duplicate workdir accepted")
	}

	// Re-provisioning the instance that owns the folder is fine.
	if err := checkExistingWorkdir("first", dir); err != nil {
		t.Fatalf("owner re-provision refused: %v", err)
	}
}

func TestCreateExistingWorkdirRefusesBeforeProvisioning(t *testing.T) {
	envDir := withEnvDir(t)
	dir := t.TempDir()
	writeInstanceEnv(t, envDir, "first", dir)

	missing := filepath.Join(t.TempDir(), "nope")
	for _, wd := range []string{missing, dir} {
		_, err := Create(Options{InstanceName: "second", Agent: "claude-code", Workdir: wd, RunUser: "nobody", ExistingWorkdir: true})
		if err == nil {
			t.Fatalf("Create with workdir %s succeeded", wd)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("refused create still made %s", missing)
	}
	if _, err := os.Stat(filepath.Join(envDir, "second.env")); !os.IsNotExist(err) {
		t.Fatal("refused create wrote a registry file")
	}
}
