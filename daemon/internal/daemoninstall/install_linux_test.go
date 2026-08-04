package daemoninstall

import (
	"fmt"
	"strings"
	"testing"
)

func TestDoctorSystemdScheduleFollowsRefresh(t *testing.T) {
	service := fmt.Sprintf(doctorUnitTemplate, binPath)
	for _, want := range []string{
		"After=agentmuxd.service network-online.target",
		"ExecStart=/usr/local/bin/agentmux doctor",
		"TimeoutStartSec=15min",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service is missing %q", want)
		}
	}
	timer := fmt.Sprintf(doctorTimerTemplate, "03:30")
	if !strings.Contains(timer, "OnCalendar=*-*-* 03:30:00 Australia/Perth") {
		t.Fatal("doctor timer must run after the default 03:00 refresh")
	}
	if !strings.Contains(timer, "Persistent=true") {
		t.Fatal("doctor timer should catch up after host downtime")
	}
}

func TestDoctorSystemdNamesDoNotOverlapInstanceUnits(t *testing.T) {
	if strings.HasPrefix(doctorUnitName, "agentmux-") || strings.HasPrefix(doctorTimerName, "agentmux-") {
		t.Fatalf("doctor units overlap the agentmux-<instance> namespace: %s / %s", doctorUnitName, doctorTimerName)
	}
}
