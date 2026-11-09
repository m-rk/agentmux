package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// The self-update ops are host-wide like gc: the grant names the op,
// with no session target to match.
func TestSelfUpdateOpsNeedAGrant(t *testing.T) {
	h := newHarness(t, all("list"))
	for _, tc := range []struct{ op, body string }{
		{gatewayapi.OpShipPublish, `{"commits":{"agentmux":"2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"}}`},
		{gatewayapi.OpVersions, `{}`},
		{gatewayapi.OpSelfUpdateLog, `{}`},
	} {
		if rec := h.post(tc.op, tc.body); rec.Code != 403 {
			t.Errorf("%s without grant: %d %s", tc.op, rec.Code, rec.Body)
		}
	}
}

func TestShipPublishMergesAndEchoes(t *testing.T) {
	h := newHarness(t, all(gatewayapi.OpShipPublish))
	sha := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	rec := h.post(gatewayapi.OpShipPublish, `{"commits":{"agentmux":"`+sha+`"}}`)
	if rec.Code != 200 {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	var res gatewayapi.ShipPublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Shipped["agentmux"] != sha {
		t.Errorf("shipped = %+v", res.Shipped)
	}
	// An empty body stands for {} (decode's emptyOK), so a bare POST
	// reports the gate without changing it.
	rec = h.post(gatewayapi.OpShipPublish, ``)
	var res2 gatewayapi.ShipPublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res2); err != nil {
		t.Fatal(err)
	}
	if res2.Shipped["agentmux"] != sha {
		t.Errorf("after empty publish, shipped = %+v", res2.Shipped)
	}
}

func TestVersionsReportsHost(t *testing.T) {
	h := newHarness(t, all(gatewayapi.OpVersions))
	sha := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	h.backend.versions = map[string]string{"agentmux": sha}
	rec := h.post(gatewayapi.OpVersions, `{}`)
	if rec.Code != 200 {
		t.Fatalf("versions: %d %s", rec.Code, rec.Body)
	}
	var res gatewayapi.VersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Host != "hostA" || res.Versions["agentmux"] != sha {
		t.Errorf("versions = %+v", res)
	}
}

func TestSelfUpdateLogTails(t *testing.T) {
	h := newHarness(t, all(gatewayapi.OpSelfUpdateLog))
	sha := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	h.backend.logLines = []string{"a", "deployed agentmux@" + sha + " old=none new=" + sha}
	rec := h.post(gatewayapi.OpSelfUpdateLog, `{"lines":1}`)
	if rec.Code != 200 {
		t.Fatalf("log: %d %s", rec.Code, rec.Body)
	}
	var res gatewayapi.SelfUpdateLogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "deployed agentmux@"+sha) {
		t.Errorf("lines = %+v", res.Lines)
	}
}

func TestSelfUpdateOpsAreKnownOps(t *testing.T) {
	h := newHarness(t, ValidGrants([]gatewayapi.Grant{
		{Ops: []string{gatewayapi.OpVersions}, Sessions: []string{"*@*"}},
		{Ops: []string{"list", "teleport"}, Sessions: []string{"*@*"}},
	})...)
	// versions survives ValidGrants (it is a known op); teleport is
	// dropped whole, so list with it grants nothing.
	if rec := h.post(gatewayapi.OpVersions, `{}`); rec.Code != 200 {
		t.Errorf("versions with valid grant: %d %s", rec.Code, rec.Body)
	}
	if rec := h.post("list", `{}`); rec.Code != 403 {
		t.Errorf("list with dropped grant: %d %s", rec.Code, rec.Body)
	}
}

func TestVersionsCarriesHostWarnings(t *testing.T) {
	h := newHarness(t, all(gatewayapi.OpVersions))
	h.backend.versions = map[string]string{"agentmux": "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"}
	h.backend.warnings = []string{"agentmux installed 2fe1a23 lags shipped 9999999 for 45 minutes: the host's updater is not installing it"}
	rec := h.post(gatewayapi.OpVersions, `{}`)
	var res gatewayapi.VersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "lags shipped") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestVersionWarningsFromStateFiles(t *testing.T) {
	home := t.TempDir()
	shipped, versions, _ := SelfUpdateFiles(home)
	if err := os.MkdirAll(filepath.Dir(shipped), 0o755); err != nil {
		t.Fatal(err)
	}
	shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for path, sha := range map[string]string{shipped: shaB, versions: shaA} {
		if err := selfupdate.SaveCommits(path, map[string]string{"agentmux": sha}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if got := VersionWarnings(home, now, nil); len(got) != 0 {
		t.Errorf("just shipped warned: %v", got)
	}
	old := now.Add(-time.Hour)
	if err := os.Chtimes(shipped, old, old); err != nil {
		t.Fatal(err)
	}
	got := VersionWarnings(home, now, selfupdate.ParseLaunchctlList("-\t78\tcom.agentmux.x\n"))
	if len(got) != 2 || !strings.Contains(got[0], "lags shipped") || !strings.Contains(got[1], "com.agentmux.x (exit 78)") {
		t.Errorf("warnings = %v, want lag then stuck job", got)
	}
}
