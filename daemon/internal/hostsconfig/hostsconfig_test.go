package hostsconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/address"
)

func TestCheckUnique(t *testing.T) {
	ok := []Host{{Name: "local"}, {Name: "build-box"}, {Name: "other"}}
	if err := CheckUnique(ok); err != nil {
		t.Fatalf("distinct hosts: %v", err)
	}
	if err := CheckUnique([]Host{{Name: "build-box"}, {Name: "Build-Box"}}); err == nil {
		t.Fatal("case-folded duplicate accepted")
	}
	self := address.LocalHostName()
	err := CheckUnique([]Host{{Name: "local"}, {Name: self}})
	if err == nil || !strings.Contains(err.Error(), self) {
		t.Fatalf("local plus this machine's own name: got %v", err)
	}
}

func TestLoadGateway(t *testing.T) {
	write := func(gw string) string {
		p := filepath.Join(t.TempDir(), "hosts.yaml")
		body := "hosts:\n  - name: box\n    address: tcp://100.64.0.1:4287\n"
		if gw != "" {
			body += "    gateway: " + gw + "\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, good := range []string{"", "http://100.64.0.1:4288", "https://box.example.ts.net", "http://box:4288/"} {
		cfg, err := Load(write(good))
		if err != nil {
			t.Fatalf("gateway %q: %v", good, err)
		}
		if cfg.Hosts[0].Gateway != good {
			t.Fatalf("gateway %q loaded as %q", good, cfg.Hosts[0].Gateway)
		}
	}
	for _, bad := range []string{
		"100.64.0.1:4288", "ftp://box:4288", "http://", "http://box:4288/v1",
		"http://box:4288?x=1", "http://user:pw@box:4288", "http://box:4288#f", "box",
	} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("gateway %q accepted", bad)
		}
	}
}

func TestLoadAddressOrGateway(t *testing.T) {
	load := func(entry string) error {
		p := filepath.Join(t.TempDir(), "hosts.yaml")
		if err := os.WriteFile(p, []byte("hosts:\n  - name: box\n"+entry), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		return err
	}
	if err := load("    gateway: http://100.64.0.1:4288\n"); err != nil {
		t.Fatalf("gateway only: %v", err)
	}
	if err := load("    address: tcp://100.64.0.1:4287\n"); err != nil {
		t.Fatalf("address only: %v", err)
	}
	if err := load(""); err == nil {
		t.Fatal("neither address nor gateway accepted")
	}
}
