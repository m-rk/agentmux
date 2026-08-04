package daemoninstall

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
)

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
