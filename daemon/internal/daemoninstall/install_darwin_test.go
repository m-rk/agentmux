package daemoninstall

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
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

func TestProbeLaunchAgentRunsHourly(t *testing.T) {
	plist := fmt.Sprintf(probePlistTemplate, probeLabel, "/tmp/agentmux", "/tmp/log", "/tmp/log")
	if err := xml.Unmarshal([]byte(plist), new(any)); err != nil {
		t.Fatalf("probe plist is not valid XML: %v", err)
	}
	for _, want := range []string{"<string>models</string>\n        <string>probe</string>", "<key>StartInterval</key>\n    <integer>3600</integer>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist is missing %q", want)
		}
	}
	if strings.HasPrefix(probeLabel, "com.agentmux.") {
		t.Fatalf("probe label overlaps the com.agentmux.<instance> namespace: %s", probeLabel)
	}
}

// The daemon, doctor, gc and probe jobs start the binary through the
// launcher, so an update does not change what launchd registered (AMUX-143).
func TestLaunchAgentsUseLauncher(t *testing.T) {
	for name, plist := range map[string]string{
		"daemon": fmt.Sprintf(plistTemplate, label, "/tmp/agentmux", "/tmp/sock", "/tmp/log", "/tmp/log"),
		"doctor": fmt.Sprintf(doctorPlistTemplate, doctorLabel, "/tmp/agentmux", 3, 30, "/tmp/log", "/tmp/log"),
		"gc":     fmt.Sprintf(gcPlistTemplate, gcLabel, "/tmp/agentmux", "/tmp/log", "/tmp/log"),
		"probe":  fmt.Sprintf(probePlistTemplate, probeLabel, "/tmp/agentmux", "/tmp/log", "/tmp/log"),
	} {
		argv := launchagent.ProgramArguments([]byte(plist))
		if !launchagent.Launched(argv) || argv[3] != "/tmp/agentmux" {
			t.Errorf("%s ProgramArguments = %q", name, argv)
		}
	}
}
