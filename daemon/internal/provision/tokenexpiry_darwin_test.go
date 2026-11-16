package provision

import (
	"errors"
	"testing"
	"time"
)

func TestClaudeLoginExpiryStatus(t *testing.T) {
	now := time.Now()

	ok := claudeLoginExpiryStatus(nil, now)
	if !ok.Supported || !ok.RefreshExpiresAt.IsZero() {
		t.Errorf("logged in = %+v, want Supported with no expiry", ok)
	}

	out := claudeLoginExpiryStatus(errors.New("not logged in"), now)
	if !out.Supported || out.RefreshExpiresAt.After(now) || out.RefreshExpiresAt.IsZero() {
		t.Errorf("logged out = %+v, want Supported with expiry at or before now", out)
	}
}
