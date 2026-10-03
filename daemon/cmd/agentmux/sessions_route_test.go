package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

func writeHosts(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const routeHosts = `hosts:
  - name: local
    address: unix:///x.sock
  - name: Build-Box
    address: tcp://100.64.0.2:4287
    gateway: http://100.64.0.2:4288
  - name: nogw
    address: tcp://100.64.0.3:4287
`

func TestResolveRoute(t *testing.T) {
	hosts := writeHosts(t, routeHosts)
	missing := filepath.Join(t.TempDir(), "none.yaml")

	cases := []struct {
		name, addr, hosts string
		remote            string // expected gateway URL; "" for local
		reason            safesend.Reason
		detail            string
	}{
		{name: "local host", addr: "a@me", hosts: hosts},
		{name: "local host, no hosts file", addr: "a@me", hosts: missing},
		{name: "local with thread", addr: "a@me#T-1", hosts: hosts},
		{name: "unparseable stays local", addr: "garbage", hosts: hosts},
		{name: "remote with gateway", addr: "a@build-box", hosts: hosts, remote: "http://100.64.0.2:4288"},
		{name: "remote host case-folded", addr: "a@BUILD-BOX#T-9", hosts: hosts, remote: "http://100.64.0.2:4288"},
		{name: "entry without gateway", addr: "a@nogw", hosts: hosts, reason: safesend.ReasonNotLocal, detail: "gateway:"},
		{name: "unknown host", addr: "a@other", hosts: hosts, reason: safesend.ReasonNotLocal, detail: `"other"`},
		{name: "no hosts file", addr: "a@other", hosts: missing, reason: safesend.ReasonNotLocal, detail: "gateway:"},
		{name: "empty hosts path", addr: "a@other", hosts: "", reason: safesend.ReasonNotLocal, detail: "hosts.yaml"},
	}
	for _, c := range cases {
		route, err := resolveRoute(c.addr, c.hosts, "me")
		if c.reason != "" {
			e, ok := err.(*ops.Error)
			if !ok || e.Reason != c.reason || !strings.Contains(e.Detail, c.detail) {
				t.Errorf("%s: got %#v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.remote == "" && route.Remote != nil {
			t.Errorf("%s: routed remote", c.name)
		}
		if c.remote != "" && (route.Remote == nil || route.Remote.BaseURL != c.remote) {
			t.Errorf("%s: got %+v", c.name, route.Remote)
		}
	}
}

func TestResolveRouteLocalAlias(t *testing.T) {
	// "local" in hosts.yaml names this machine, so its sessions are local and
	// never reach the hosts file.
	route, err := resolveRoute("a@me", writeHosts(t, routeHosts), "me")
	if err != nil || route.Remote != nil {
		t.Fatalf("%+v %v", route, err)
	}
}

func TestResolveRouteBrokenHostsFile(t *testing.T) {
	broken := writeHosts(t, "hosts: [not a list of hosts")
	if _, err := resolveRoute("a@me", broken, "me"); err != nil {
		t.Fatalf("local address read the hosts file: %v", err)
	}
	_, err := resolveRoute("a@box", broken, "me")
	if e := ops.AsError(err); e.Reason != safesend.ReasonFailed || !strings.Contains(e.Detail, "box") {
		t.Fatalf("got %#v", err)
	}
	badURL := writeHosts(t, "hosts:\n  - name: box\n    address: tcp://x:1\n    gateway: ftp://x\n")
	if _, err := resolveRoute("a@box", badURL, "me"); err == nil {
		t.Fatal("bad gateway URL accepted")
	}
}

func TestRemoteSendRequest(t *testing.T) {
	got := remoteSendRequest(ops.SendRequest{
		Address: "a@box", Text: "hi", Via: "sent", By: "me", From: "t1", Correlation: "c",
		Wait: 90 * time.Second, Confirm: 15 * time.Second,
	})
	if got.Address != "a@box" || got.Text != "hi" || got.Via != "sent" || got.From != "t1" || got.Correlation != "c" ||
		got.WaitSeconds != 90 || got.ConfirmSeconds != 15 {
		t.Fatalf("got %+v", got)
	}
}
