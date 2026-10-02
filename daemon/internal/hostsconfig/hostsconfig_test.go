package hostsconfig

import (
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
