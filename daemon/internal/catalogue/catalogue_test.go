package catalogue

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../../docs/examples/catalogue.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Offerings) != 4 {
		t.Fatalf("got %d offerings", len(c.Offerings))
	}
	if c.Offerings[2].Icon != "amp" || c.Offerings[0].Icon != "claude" {
		t.Errorf("icons: %q %q", c.Offerings[0].Icon, c.Offerings[2].Icon)
	}
	if c.Fuel[0].CapSource != "manual" {
		t.Errorf("fuel: %+v", c.Fuel)
	}
}

func TestJSONShapeIsStable(t *testing.T) {
	o := Offering{ID: "a", Runner: RunnerAmp, Icon: "amp", Label: "A", Fuel: FuelAmp, Tier: TierEasy, Mode: "m",
		Hosts:        []string{"h"},
		Availability: []HostAvailability{{Host: "h", Available: false, Reason: "unreachable"}}}
	b, _ := json.Marshal(o)
	got := string(b)
	for _, want := range []string{`"id":"a"`, `"runner":"amp"`, `"icon":"amp"`, `"label":"A"`, `"fuel":"amp"`, `"tier":"easy"`, `"mode":"m"`,
		`"hosts":[{"host":"h","available":false,"reason":"unreachable"}]`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestValidateRefusals(t *testing.T) {
	base := `offerings:
  - {id: a, runner: codex, model: m, label: A, fuel: chatgpt, tier: easy, hosts: [local]}
`
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"duplicate id":      base + "  - {id: a, runner: codex, model: m, label: A, fuel: chatgpt, tier: easy, hosts: [local]}\n",
		"unknown runner":    strings.Replace(base, "codex", "nope", 1),
		"unknown fuel":      strings.Replace(base, "chatgpt", "coal", 1),
		"tier":              strings.Replace(base, "easy", "medium", 1),
		"needs a mode":      strings.Replace(base, "runner: codex, model: m", "runner: amp", 1),
		"only for the amp":  strings.Replace(base, "model: m", "model: m, mode: x", 1),
		"hosts is required": strings.Replace(base, "hosts: [local]", "hosts: []", 1),
		"cap_percent":       base + "fuel:\n  - {name: x, cap_percent: 150}\n",
	}
	for want, in := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	c, err := Load(t.TempDir() + "/none.yaml")
	if err != nil || len(c.Offerings) != 0 {
		t.Fatalf("%v %v", c, err)
	}
}

func TestIconKeysAreStable(t *testing.T) {
	want := map[string]string{"claude-code": "claude", "codex": "codex", "amp": "amp", "opencode": "opencode", "kilo": "kilo"}
	for r, k := range want {
		if IconKey(r) != k {
			t.Errorf("%s: %q", r, IconKey(r))
		}
	}
	if IconKey("unknown") != "" {
		t.Error("unknown runner has an icon")
	}
}
