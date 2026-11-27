package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The daily gc job runs `gc -host local`. With a hosts.yaml that lists a
// peer behind a gateway, it must still collect this machine only: the peer's
// gateway refuses gc, and that refusal failed the job every day (AMUX-145).
func TestGCHostsLocalIgnoresHostsFile(t *testing.T) {
	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(hostsPath, []byte("hosts:\n  - name: hub\n    gateway: http://hub.invalid:4288\n  - name: local\n    address: unix:///var/empty/other.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "agentmuxd.sock")
	got, err := gcHosts("local", hostsPath, sock)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "local" || got[0].Gateway != "" || got[0].Address != "unix://"+sock {
		t.Fatalf("gcHosts(local) = %+v, want only this machine on its own socket", got)
	}

	all, err := gcHosts("all", hostsPath, sock)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("gcHosts(all) = %+v, want both hosts.yaml entries", all)
	}
	one, err := gcHosts("hub", hostsPath, sock)
	if err != nil || len(one) != 1 || one[0].Gateway == "" {
		t.Fatalf("gcHosts(hub) = %+v, %v", one, err)
	}
	if _, err := gcHosts("nope", hostsPath, sock); err == nil {
		t.Fatal("gcHosts(nope) should fail")
	}
}

// Without a hosts.yaml, "local" still works.
func TestGCHostsLocalWithoutHostsFile(t *testing.T) {
	dir := t.TempDir()
	got, err := gcHosts("local", filepath.Join(dir, "missing.yaml"), filepath.Join(dir, "s.sock"))
	if err != nil || len(got) != 1 || got[0].Name != "local" {
		t.Fatalf("gcHosts(local) = %+v, %v", got, err)
	}
}
