package main

import (
	"strings"
	"testing"
)

func TestRenderPaseoUpdateUnits(t *testing.T) {
	service, timer := renderPaseoUpdateUnits("ubuntu", "/usr/local/bin/agentmux", "04:00")
	if !strings.Contains(service, "ExecStart=/usr/local/bin/agentmux paseo update -run-user ubuntu") {
		t.Errorf("service missing ExecStart:\n%s", service)
	}
	if !strings.Contains(timer, "OnCalendar=*-*-* 04:00:00 Australia/Perth") {
		t.Errorf("timer missing schedule:\n%s", timer)
	}
}

func TestRenderPaseoUpdatePlist(t *testing.T) {
	plist, err := renderPaseoUpdatePlist("/Users/me/.agentmux/bin/agentmux", "04:05", "/Users/me/.agentmux/log")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>paseo</string>", "<string>update</string>", "<integer>4</integer>", "<integer>5</integer>", "paseo-update.err.log"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	if _, err := renderPaseoUpdatePlist("b", "nope", "l"); err == nil {
		t.Error("expected error for bad time")
	}
}
