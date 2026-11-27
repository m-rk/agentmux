package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jobLastErr finds a job's stderr log through its plist and quotes the
// last line, so a failed scheduled job's warning says why (AMUX-145).
func TestJobLastErr(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	errLog := filepath.Join(dir, "gc.err.log")
	body := strings.Repeat("old noise\n", 1000) + "gc on hub: forbidden: gc is not permitted\n"
	if err := os.WriteFile(errLog, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	plist := `<plist version="1.0"><dict>
    <key>Label</key>
    <string>com.m-rk.agentmux.gc</string>
    <key>StandardErrorPath</key>
    <string>` + errLog + `</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(agents, "com.m-rk.agentmux.gc.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := jobLastErr(agents, "com.m-rk.agentmux.gc"); got != "gc on hub: forbidden: gc is not permitted" {
		t.Errorf("jobLastErr = %q", got)
	}
	if got := jobLastErr(agents, "com.m-rk.agentmux.missing"); got != "" {
		t.Errorf("missing plist = %q", got)
	}
	if got := jobLastErr(agents, "com.example.other"); got != "" {
		t.Errorf("foreign label = %q", got)
	}
	if got := jobLastErr(agents, "com.agentmux.../../x"); got != "" {
		t.Errorf("label with a path = %q", got)
	}
}
