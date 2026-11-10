package catalogue

import (
	"strings"
	"testing"
	"time"
)

func TestBuildNeverDropsAFailedHost(t *testing.T) {
	c, err := Parse([]byte(`offerings:
  - {id: a, runner: codex, model: m, label: A, fuel: chatgpt, tier: easy, hosts: [local, far, gone, bare]}
`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cat := Build(c, map[string]HostReport{
		"local": {Runners: map[string]bool{"codex": true}, CheckedAt: now},
		"far":   {Err: "host unreachable", CheckedAt: now},
		"bare":  {Runners: map[string]bool{"codex": false}, CheckedAt: now},
	}, "box1")
	av := cat.Offerings[0].Availability
	if len(av) != 4 {
		t.Fatalf("got %d results: %+v", len(av), av)
	}
	if av[0].Label != "box1" || av[1].Label != "far" {
		t.Errorf("labels: %q %q", av[0].Label, av[1].Label)
	}
	if !av[0].Available || av[0].CheckedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("local: %+v", av[0])
	}
	if av[1].Available || av[1].Reason != "host unreachable" {
		t.Errorf("far: %+v", av[1])
	}
	if av[2].Available || !strings.Contains(av[2].Reason, "not checked") {
		t.Errorf("gone: %+v", av[2])
	}
	if av[3].Available || av[3].Reason != "codex is not installed" {
		t.Errorf("bare: %+v", av[3])
	}
	if cat.Version != FormatVersion || cat.Offerings[0].Icon != "codex" {
		t.Errorf("cat: %+v", cat)
	}
}

func TestHostsSortedUnique(t *testing.T) {
	c := Config{Offerings: []Offering{{Hosts: []string{"b", "a"}}, {Hosts: []string{"a", "c"}}}}
	if got := strings.Join(c.Hosts(), ","); got != "a,b,c" {
		t.Errorf("hosts = %s", got)
	}
}
