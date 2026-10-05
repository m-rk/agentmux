package main

import (
	"strings"
	"testing"
)

func TestRenderGatewayUnit(t *testing.T) {
	args := gatewayRunArgs("100.64.0.1:4288", "example.com/cap/agentmux-gateway", "", "/usr/bin/tailscale")
	unit := renderGatewayUnit("ubuntu", "/usr/local/bin/agentmux", args)
	for _, want := range []string{
		"User=ubuntu",
		"ExecStart=/usr/local/bin/agentmux gateway run -capability example.com/cap/agentmux-gateway -listen 100.64.0.1:4288 -tailscale /usr/bin/tailscale\n",
		"Restart=always",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	if unit := renderGatewayUnit("u", "/opt/my agentmux/bin", args[:1]); !strings.Contains(unit, `ExecStart="/opt/my agentmux/bin" gateway`) {
		t.Errorf("path with space not quoted:\n%s", unit)
	}
}

func TestRenderGatewayPlist(t *testing.T) {
	args := gatewayRunArgs("", "example.com/cap/agentmux-gateway", "/run/a&b.sock", "")
	plist := renderGatewayPlist("/home/me/.agentmux/bin/agentmux", args, "/home/me/.agentmux/log")
	for _, want := range []string{
		"<string>com.m-rk.agentmux.gateway</string>",
		"<string>/home/me/.agentmux/bin/agentmux</string>",
		"<string>gateway</string>", "<string>run</string>",
		"<string>example.com/cap/agentmux-gateway</string>",
		"<string>/run/a&amp;b.sock</string>",
		"<key>KeepAlive</key>",
		"/home/me/.agentmux/log/gateway.err.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "-listen") {
		t.Error("no -listen was given")
	}
}

func TestCheckListenAddr(t *testing.T) {
	cases := []struct {
		addr string
		test bool
		ok   bool
	}{
		{"100.64.0.1:4288", false, true},
		{"100.127.255.254:4288", false, true},
		{"[fd7a:115c:a1e0::1]:4288", false, true},
		{"100.128.0.1:4288", false, false},
		{"100.63.255.255:4288", false, false},
		{"192.168.1.5:4288", false, false},
		{"0.0.0.0:4288", false, false},
		{"[::]:4288", false, false},
		{"localhost:4288", false, false},
		{"100.64.0.1", false, false},
		{"127.0.0.1:4288", false, false},
		{"127.0.0.1:4288", true, true},
		{"[::1]:4288", true, true},
		{"100.64.0.1:4288", true, false},
		{"0.0.0.0:4288", true, false},
	}
	for _, c := range cases {
		if err := checkListenAddr(c.addr, c.test); (err == nil) != c.ok {
			t.Errorf("checkListenAddr(%q, %v) = %v, want ok=%v", c.addr, c.test, err, c.ok)
		}
	}
}

func TestParseRate(t *testing.T) {
	if l, err := parseRate("30/min", 10); err != nil || l.PerMinute != 30 || l.Burst != 10 {
		t.Errorf("%+v %v", l, err)
	}
	if l, err := parseRate("0", 0); err != nil || l.PerMinute != 0 {
		t.Errorf("%+v %v", l, err)
	}
	for _, bad := range []string{"", "x", "-1/min", "5/sec"} {
		if _, err := parseRate(bad, 1); err == nil {
			t.Errorf("parseRate(%q) should fail", bad)
		}
	}
	if _, err := parseRate("5", 0); err == nil {
		t.Error("zero burst should fail")
	}
}
