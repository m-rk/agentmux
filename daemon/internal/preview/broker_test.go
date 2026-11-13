package preview

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthorize(t *testing.T) {
	reg := func(fields map[string]string, err error) func(string) (map[string]string, error) {
		return func(string) (map[string]string, error) { return fields, err }
	}
	uid := func(name string) (string, error) {
		if name == "worker" {
			return "1001", nil
		}
		return "", errors.New("no such user")
	}
	good := map[string]string{"AGENTMUX_INSTANCE_NAME": "task-a", "AGENTMUX_WORKDIR": "/w/task-a", "AGENTMUX_RUN_USER": "worker"}
	got, err := Authorize("task-a", "1001", reg(good, nil), uid)
	if err != nil || got.Workspace != "/w/task-a" || got.RunUser != "worker" {
		t.Fatalf("good: %+v, %v", got, err)
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range good {
			m[a] = b
		}
		m[k] = v
		return m
	}
	cases := map[string]struct {
		instance, caller string
		fields           map[string]string
		regErr           error
		want             string
	}{
		"root caller":       {"task-a", "0", good, nil, "non-root"},
		"empty caller":      {"task-a", "", good, nil, "non-root"},
		"not a task":        {"forgentic", "1001", good, nil, "task-*"},
		"path in name":      {"task-../x", "1001", good, nil, "task-*"},
		"no registry":       {"task-a", "1001", nil, errors.New("gone"), "reading task registry"},
		"identity mismatch": {"task-a", "1001", with("AGENTMUX_INSTANCE_NAME", "task-b"), nil, "identity"},
		"relative workdir":  {"task-a", "1001", with("AGENTMUX_WORKDIR", "rel"), nil, "workdir"},
		"no run user":       {"task-a", "1001", with("AGENTMUX_RUN_USER", ""), nil, "run user"},
		"unknown user":      {"task-a", "1001", with("AGENTMUX_RUN_USER", "ghost"), nil, "task user"},
		"other caller":      {"task-a", "1002", good, nil, "does not own"},
	}
	for name, c := range cases {
		_, err := Authorize(c.instance, c.caller, reg(c.fields, c.regErr), uid)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want error containing %q", name, err, c.want)
		}
	}
}

func TestGateOnePerInstanceAndHostCap(t *testing.T) {
	g := NewGate(2)
	relA, why := g.Acquire("task-a")
	if relA == nil {
		t.Fatal(why)
	}
	if rel, why := g.Acquire("task-a"); rel != nil || !strings.Contains(why, "already running") {
		t.Errorf("second capture for the same instance: %q", why)
	}
	relB, why := g.Acquire("task-b")
	if relB == nil {
		t.Fatal(why)
	}
	if rel, why := g.Acquire("task-c"); rel != nil || !strings.Contains(why, "busy") {
		t.Errorf("over the host cap: %q", why)
	}
	relA()
	relC, why := g.Acquire("task-c")
	if relC == nil {
		t.Fatalf("slot not freed: %s", why)
	}
	relA2, why := g.Acquire("task-a")
	if relA2 != nil {
		t.Errorf("cap should be full again")
	}
	_ = why
	relB()
	relC()
}
