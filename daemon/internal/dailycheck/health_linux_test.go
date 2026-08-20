package dailycheck

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDescribeFailureIncludesAppsOwnLogLine guards against a real incident:
// a broken opencode install made every nightly refresh fail with "fork/exec
// ...: exec format error", but doctor's report showed only systemd
// bookkeeping (active=inactive sub=dead result=exit-code exit=1
// restarts=0) — diagnosing it required a manual journalctl dig. Confirms
// the app's own last log line gets folded into the reported detail.
func TestDescribeFailureIncludesAppsOwnLogLine(t *testing.T) {
	previous := lastAppLogLine
	lastAppLogLine = func(ctx context.Context, unit string) (string, error) {
		return "2026/08/20 19:00:55 session update ken: opencode update/check failed, leaving existing session running untouched: fork/exec /home/ubuntu/.npm-global/bin/opencode: exec format error", nil
	}
	t.Cleanup(func() { lastAppLogLine = previous })

	props := map[string]string{"ActiveState": "inactive", "SubState": "dead", "Result": "exit-code", "ExecMainStatus": "1", "NRestarts": "0"}
	detail := describeFailure(context.Background(), "agentmux-ken-update.service", props)

	if !strings.Contains(detail,"exec format error") {
		t.Errorf("describeFailure = %q, want it to include the app's own log line", detail)
	}
	if !strings.Contains(detail,"active=inactive") {
		t.Errorf("describeFailure = %q, want it to still include the systemd property summary", detail)
	}
}

// TestDescribeFailureFallsBackWithoutALogLine guards the case where
// journalctl has nothing (rotated out, or permission denied): doctor must
// still report the systemd property summary rather than an empty detail.
func TestDescribeFailureFallsBackWithoutALogLine(t *testing.T) {
	previous := lastAppLogLine
	lastAppLogLine = func(ctx context.Context, unit string) (string, error) {
		return "", errors.New("no journal entries")
	}
	t.Cleanup(func() { lastAppLogLine = previous })

	props := map[string]string{"ActiveState": "inactive", "SubState": "dead", "Result": "exit-code", "ExecMainStatus": "1", "NRestarts": "0"}
	detail := describeFailure(context.Background(), "agentmux-ken-update.service", props)

	if detail != describeProperties(props) {
		t.Errorf("describeFailure = %q, want the plain property summary when no log line is available", detail)
	}
}
