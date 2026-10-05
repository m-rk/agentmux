//go:build linux

package provision

import (
	"os/user"
	"strings"
	"testing"
)

// TestAmpOpAuthProbeLinuxNoEnvFileFallsBack verifies the Linux probe
// honors the run user's home: with no env-file there it returns nil (the
// caller falls back to probing the stored `amp login`), and an unknown
// run user is an error naming the user rather than a nil probe that would
// silently check the wrong account.
// TestAmpRunnerIDAlwaysHasHostSuffix documents the cross-host uniqueness
// invariant at the derivation level: the runner ID the provisioner stores
// must differ per host. Both amp provisioners fall back to DefaultHostName
// when no explicit or remembered host exists, so in practice the stored ID
// always carries a suffix; an empty host keeps the historical plain
// derivation for already-stored IDs.
func TestAmpRunnerIDAlwaysHasHostSuffix(t *testing.T) {
	plain, err := AmpRunnerIDForInstance("site-amp", "amp", "")
	if err != nil {
		t.Fatal(err)
	}
	if plain != "site" {
		t.Errorf("empty-host derivation = %q, want the historical %q", plain, "site")
	}
	hosted, err := AmpRunnerIDForInstance("site-amp", "amp", "somehost")
	if err != nil {
		t.Fatal(err)
	}
	if hosted == plain {
		t.Errorf("hosted derivation = %q, same as the unsuffixed ID", hosted)
	}
}

func TestAmpOpAuthProbeLinuxNoEnvFileFallsBack(t *testing.T) {
	self, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	probe, err := ampOpAuthProbe(self.Username, "agentmux-test-no-such-instance")
	if err != nil {
		t.Fatalf("ampOpAuthProbe without an env-file = error %v, want (nil, nil)", err)
	}
	if probe != nil {
		t.Errorf("ampOpAuthProbe without an env-file = %v, want nil (fall back to stored login)", probe)
	}
	if _, err := ampOpAuthProbe("agentmux-test-no-such-user", "x"); err == nil {
		t.Error("ampOpAuthProbe with an unknown run user = nil error, want an error")
	} else if !strings.Contains(err.Error(), "agentmux-test-no-such-user") {
		t.Errorf("unknown-user error = %q, want it to name the user", err)
	}
}
