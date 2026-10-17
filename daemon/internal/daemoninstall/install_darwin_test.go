package daemoninstall

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReloadGatewayAgentRegistersExistingJob(t *testing.T) {
	temp := t.TempDir()
	plist := filepath.Join(temp, "gateway.plist")
	if err := os.WriteFile(plist, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(temp, "calls")
	bin := filepath.Join(temp, "launchctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + log + "\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", temp+":"+os.Getenv("PATH"))
	if err := reloadGatewayAgent("gui/501", plist); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "bootout gui/501 " + plist + "\nbootstrap gui/501 " + plist + "\n"
	if string(calls) != want {
		t.Fatalf("launchctl calls = %q, want %q", calls, want)
	}
}

func TestDoctorLaunchAgentScheduleFollowsRefresh(t *testing.T) {
	plist := fmt.Sprintf(doctorPlistTemplate, doctorLabel, "/tmp/agentmux", 3, 30, "/tmp/log", "/tmp/log")
	if err := xml.Unmarshal([]byte(plist), new(any)); err != nil {
		t.Fatalf("doctor plist is not valid XML: %v", err)
	}
	for _, want := range []string{
		"<string>doctor</string>",
		"<key>Hour</key>\n        <integer>3</integer>",
		"<key>Minute</key>\n        <integer>30</integer>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist is missing %q", want)
		}
	}
}

func TestDoctorLaunchAgentDoesNotOverlapInstanceLabels(t *testing.T) {
	if strings.HasPrefix(doctorLabel, "com.agentmux.") {
		t.Fatalf("doctor label overlaps the com.agentmux.<instance> namespace: %s", doctorLabel)
	}
}
