package session

import (
	"os"
	"path/filepath"
	"testing"
)

// The nightly update of a stopped instance (unit disabled by `control stop`)
// does nothing: no claude update, no compact, no restart (AMUX-129). With
// no stub for the claude/systemctl calls, reaching past the guard would
// fail on the missing run user, so a nil return proves the early exit.
func TestUpdateSkipsStoppedInstance(t *testing.T) {
	dir := withEnvDir(t)
	reg := "AGENTMUX_RUN_USER=nobody-such-user\nAGENTMUX_WORKDIR=/nonexistent\n"
	for _, name := range []string{"cc", "ag", "ampi"} {
		if err := os.WriteFile(filepath.Join(dir, name+".env"), []byte(reg+"AGENTMUX_AGENT=x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := unitDisabled
	unitDisabled = func(string) bool { return true }
	defer func() { unitDisabled = old }()

	if err := updateClaudeCode("cc"); err != nil {
		t.Errorf("claude update of a stopped instance = %v, want nil", err)
	}
	if err := updateAgentmux("ag"); err != nil {
		t.Errorf("agentmux update of a stopped instance = %v, want nil", err)
	}
	if err := updateAmp("ampi"); err != nil {
		t.Errorf("amp update of a stopped instance = %v, want nil", err)
	}

	// Not disabled: the update goes on and trips over the unknown user.
	unitDisabled = func(string) bool { return false }
	if err := updateClaudeCode("cc"); err == nil {
		t.Error("claude update of an enabled instance returned nil; the guard is too broad")
	}
}
