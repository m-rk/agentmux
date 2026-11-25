package probe

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/usagehook"
)

var at = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// fakeTries answers each provider from a script of results, one per try.
type fakeTries struct {
	mu     sync.Mutex
	script map[string][]*Failure
	calls  map[string]int
	slept  []time.Duration
	logged int
}

func (f *fakeTries) opts() Options {
	return Options{
		Try: func(_ context.Context, t Target) Attempt {
			f.mu.Lock()
			defer f.mu.Unlock()
			n := f.calls[t.Provider]
			f.calls[t.Provider]++
			s := f.script[t.Provider]
			var err *Failure
			if n < len(s) {
				err = s[n]
			}
			return Attempt{Latency: 1500 * time.Millisecond, Tokens: usagehook.Tokens{Input: 10, Output: 1}, Err: err}
		},
		Sleep:   func(_ context.Context, d time.Duration) { f.mu.Lock(); f.slept = append(f.slept, d); f.mu.Unlock() },
		Now:     func() time.Time { return at },
		Backoff: DefaultBackoff,
		Logged:  func(Target, int, Attempt) { f.mu.Lock(); f.logged++; f.mu.Unlock() },
	}
}

func newFake(script map[string][]*Failure) *fakeTries {
	return &fakeTries{script: script, calls: map[string]int{}}
}

var quota = &Failure{Kind: catalogue.ErrKindQuota, Words: "usage limit or quota reached"}

func TestProbePassesFirstTry(t *testing.T) {
	f := newFake(nil)
	rows := Run(context.Background(), f.opts(), []Target{{Provider: "claude", Label: "Claude", Runner: "claude-code", Model: "haiku"}}, "local", "hub", nil)
	r := rows[0]
	if r.Status != catalogue.HealthOK || r.Attempts != 1 || r.LastOKAt != "2026-11-24T12:00:00Z" || r.CheckedAt != r.LastOKAt {
		t.Errorf("row = %+v", r)
	}
	if r.LatencyMS != 1500 || r.Error != "" || r.ErrorKind != "" || r.DownSince != "" || r.HostLabel != "hub" {
		t.Errorf("row = %+v", r)
	}
	if len(f.slept) != 0 || f.logged != 1 {
		t.Errorf("slept %v, logged %d", f.slept, f.logged)
	}
}

func TestProbeRecoversOnRetry(t *testing.T) {
	f := newFake(map[string][]*Failure{"chatgpt": {quota, quota}})
	rows := Run(context.Background(), f.opts(), []Target{{Provider: "chatgpt", Runner: "codex", Model: "codex-small", Effort: "low"}}, "local", "hub", nil)
	r := rows[0]
	if r.Status != catalogue.HealthOK || r.Attempts != 3 || r.Error != "" {
		t.Errorf("a pass on the third try is a pass: %+v", r)
	}
	if len(f.slept) != 2 || f.slept[0] != DefaultBackoff[0] || f.slept[1] != DefaultBackoff[1] || f.logged != 3 {
		t.Errorf("slept %v, logged %d", f.slept, f.logged)
	}
}

func TestProbeDownAfterThreeFailures(t *testing.T) {
	prev := []catalogue.ProviderHealth{{Provider: "amp", Host: "local", Status: catalogue.HealthOK, LastOKAt: "2026-11-24T11:00:00Z"}}
	f := newFake(map[string][]*Failure{"amp": {quota, quota, quota, quota}})
	rows := Run(context.Background(), f.opts(), []Target{{Provider: "amp", Runner: "amp", Model: "rush"}}, "local", "hub", prev)
	r := rows[0]
	if r.Status != catalogue.HealthDown || r.Attempts != Tries || f.calls["amp"] != Tries {
		t.Fatalf("row = %+v, calls %d", r, f.calls["amp"])
	}
	if r.ErrorKind != catalogue.ErrKindQuota || r.Error != "usage limit or quota reached" {
		t.Errorf("error = %q/%q", r.ErrorKind, r.Error)
	}
	if r.LastOKAt != "2026-11-24T11:00:00Z" || r.DownSince != "2026-11-24T12:00:00Z" {
		t.Errorf("last ok %q, down since %q", r.LastOKAt, r.DownSince)
	}

	// The next probe, still down, keeps when the down run began; a pass
	// clears it.
	later := Record(r, Target{Provider: "amp"}, "local", "hub", Attempt{Err: quota}, 3, at.Add(time.Hour))
	if later.DownSince != "2026-11-24T12:00:00Z" {
		t.Errorf("down since moved: %+v", later)
	}
	back := Record(later, Target{Provider: "amp"}, "local", "hub", Attempt{}, 1, at.Add(2*time.Hour))
	if back.Status != catalogue.HealthOK || back.DownSince != "" || back.LastOKAt != "2026-11-24T14:00:00Z" {
		t.Errorf("recovered row = %+v", back)
	}
}

func TestMergeKeepsOtherHosts(t *testing.T) {
	prev := []catalogue.ProviderHealth{{Provider: "claude", Host: "local"}, {Provider: "claude", Host: "other"}}
	got := Merge(prev, "local", []catalogue.ProviderHealth{{Provider: "amp", Host: "local"}})
	if len(got) != 2 || got[0].Host != "other" || got[1].Provider != "amp" {
		t.Errorf("merged = %+v", got)
	}
}

const testCatalogue = `
offerings:
  - {id: opus, runner: claude-code, model: claude-big, label: Opus, fuel: claude, tier: hardest, hosts: [local, mini]}
  - {id: sol, runner: codex, model: codex-large, label: Sol, efforts: [low, high], fuel: chatgpt, tier: hard, hosts: [local]}
  - {id: luna, runner: codex, model: codex-small, label: Luna, efforts: [low, high], fuel: chatgpt, tier: easy, hosts: [local]}
  - {id: amp-hard, runner: amp, mode: deep, label: Deep, fuel: amp, tier: hard, hosts: [local]}
  - {id: amp-easy, runner: amp, mode: rush, label: Rush, fuel: amp, tier: easy, hosts: [local, mini]}
  - {id: oc-tt, runner: opencode, model: acme-llm/open-1, label: GLM, fuel: provider, tier: easy, hosts: [local]}
  - {id: oc-ol, runner: opencode, model: ollama/open-1:cloud, label: GLM cloud, fuel: provider, tier: easy, hosts: [local]}
  - {id: kilo-tt, runner: kilo, model: acme-llm/other, label: Other, fuel: provider, tier: easy, hosts: [local]}
`

func TestTargetsPickTheCheapestEntryPerProvider(t *testing.T) {
	cfg, err := catalogue.Parse([]byte(testCatalogue))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Target{}
	for _, tg := range Targets(cfg, "local") {
		got[tg.Provider] = tg
	}
	want := map[string]Target{
		"claude":   {Provider: "claude", Label: "Claude", Runner: "claude-code", Model: "haiku"},
		"chatgpt":  {Provider: "chatgpt", Label: "ChatGPT", Runner: "codex", Model: "codex-small", Effort: "low"},
		"amp":      {Provider: "amp", Label: "Amp", Runner: "amp", Model: "rush"},
		"acme-llm": {Provider: "acme-llm", Label: "acme-llm", Runner: "opencode", Model: "acme-llm/open-1"},
		"ollama":   {Provider: "ollama", Label: "ollama", Runner: "opencode", Model: "ollama/open-1:cloud"},
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %+v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	if mini := Targets(cfg, "mini"); len(mini) != 2 || mini[0].Provider != "amp" || mini[1].Provider != "claude" {
		t.Errorf("mini targets = %+v", mini)
	}
}

func TestRowsListEveryProviderOnEveryHost(t *testing.T) {
	cfg, err := catalogue.Parse([]byte(testCatalogue))
	if err != nil {
		t.Fatal(err)
	}
	reports := map[string]catalogue.HostReport{
		"local": {Probes: []catalogue.ProviderHealth{{Provider: "claude", Host: "local", Status: "ok", Model: "haiku"}}},
		"mini":  {Err: "check failed: forbidden"},
	}
	rows := Rows(cfg, reports, "hub")
	byKey := map[string]catalogue.ProviderHealth{}
	for _, r := range rows {
		byKey[r.Key()] = r
	}
	if len(rows) != 7 {
		t.Fatalf("rows = %d: %+v", len(rows), rows)
	}
	if r := byKey["claude@local"]; r.Status != "ok" || r.HostLabel != "hub" {
		t.Errorf("claude@local = %+v", r)
	}
	if r := byKey["amp@local"]; r.Status != "unknown" || r.Error != "not probed yet" {
		t.Errorf("amp@local = %+v", r)
	}
	if r := byKey["claude@mini"]; r.Status != "unknown" || r.Error != "host check failed: forbidden" || r.HostLabel != "mini" {
		t.Errorf("claude@mini = %+v", r)
	}
	reports["mini"] = catalogue.HostReport{Runners: map[string]bool{"amp": true}}
	if r := keyed(Rows(cfg, reports, "hub"))["amp@mini"]; r.Error != "host does not report probes yet" {
		t.Errorf("old host: %+v", r)
	}
	reports["mini"] = catalogue.HostReport{Probes: []catalogue.ProviderHealth{{Provider: "amp", Host: "local", Status: "down", Error: "usage limit or quota reached"}}}
	if r := keyed(Rows(cfg, reports, "hub"))["amp@mini"]; r.Status != "down" || r.Host != "mini" {
		t.Errorf("remote row: %+v", r)
	}
}

func keyed(rows []catalogue.ProviderHealth) map[string]catalogue.ProviderHealth {
	out := map[string]catalogue.ProviderHealth{}
	for _, r := range rows {
		out[r.Key()] = r
	}
	return out
}

func TestParseReadsEachRunner(t *testing.T) {
	cases := []struct {
		runner, out string
		ok          bool
		tokens      usagehook.Tokens
		session     string
	}{
		{"claude-code", `{"type":"result","is_error":false,"result":"OK","session_id":"s1","usage":{"input_tokens":9,"output_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":5}}`,
			true, usagehook.Tokens{Requests: 1, Input: 9, Output: 2, CacheRead: 100, CacheWrite: 5}, "s1"},
		{"claude-code", `{"type":"result","is_error":true,"result":"Invalid API key · Please run /login","session_id":"s2"}`, false, usagehook.Tokens{Requests: 1}, "s2"},
		{"codex", "{\"type\":\"thread.started\",\"thread_id\":\"th\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"OK\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":120,\"cached_input_tokens\":100,\"output_tokens\":5,\"reasoning_output_tokens\":3}}\n",
			true, usagehook.Tokens{Input: 20, CacheRead: 100, Output: 5, Reasoning: 3}, "th"},
		{"amp", "Signed in as a@example.com (a)\n**Individual credits:** $2.00 remaining\n", true, usagehook.Tokens{}, ""},
		{"opencode", "{\"type\":\"step_start\",\"sessionID\":\"ses\"}\n{\"type\":\"text\",\"sessionID\":\"ses\",\"part\":{\"text\":\"OK\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses\",\"part\":{\"tokens\":{\"input\":7,\"output\":1,\"reasoning\":0,\"cache\":{\"read\":2,\"write\":0}}}}\n",
			true, usagehook.Tokens{Requests: 1, Input: 7, Output: 1, CacheRead: 2}, "ses"},
		{"opencode", "{\"type\":\"text\",\"part\":{\"text\":\"I cannot do that\"}}\n", false, usagehook.Tokens{}, ""},
	}
	for i, c := range cases {
		a := Parse(c.runner, []byte(c.out))
		if (a.Err == nil) != c.ok || a.Tokens != c.tokens || a.Session != c.session {
			t.Errorf("case %d (%s): err %v tokens %+v session %q", i, c.runner, a.Err, a.Tokens, a.Session)
		}
	}
}

func TestJudgeKeepsOnlyFixedWords(t *testing.T) {
	parsed := Parse("claude-code", []byte(`{"is_error":true,"result":"Invalid API key for user@example.com at /home/someone"}`)).Err
	f := Judge(parsed, &exec.ExitError{}, false, "")
	if f == nil || f.Kind != catalogue.ErrKindAuth || strings.Contains(f.Words, "example") || strings.Contains(f.Words, "/home") {
		t.Errorf("failure = %+v", f)
	}
	if f := Judge(&Failure{}, errors.New("x"), true, ""); f.Kind != catalogue.ErrKindTimeout {
		t.Errorf("timeout = %+v", f)
	}
	if f := Judge(&Failure{}, nil, false, "dial tcp: lookup api.example: no such host"); f.Kind != catalogue.ErrKindNetwork || strings.Contains(f.Words, "example") {
		t.Errorf("network = %+v", f)
	}
	if f := Judge(&Failure{}, nil, false, "HTTP 429 Too Many Requests"); f.Kind != catalogue.ErrKindQuota {
		t.Errorf("quota = %+v", f)
	}
	if f := Judge(&Failure{}, nil, false, "something odd"); f.Kind != catalogue.ErrKindOther || f.Words != "reply was not OK" {
		t.Errorf("other = %+v", f)
	}
	if f := Judge(nil, errors.New("exit 1"), false, "warning"); f != nil {
		t.Errorf("an OK reply passes whatever the exit: %+v", f)
	}
}

func TestIsOK(t *testing.T) {
	for _, s := range []string{"OK", "ok.", " OK\n", "**OK**", "Ok!"} {
		if !IsOK(s) {
			t.Errorf("%q should be OK", s)
		}
	}
	for _, s := range []string{"", "okay then", "Not OK", "I can't"} {
		if IsOK(s) {
			t.Errorf("%q should not be OK", s)
		}
	}
}

func TestArgsAreHeadlessAndToolless(t *testing.T) {
	claude := strings.Join(Args(Target{Runner: "claude-code", Model: "haiku"}), " ")
	for _, want := range []string{"-p", "--no-session-persistence", "--model haiku", "--output-format json"} {
		if !strings.Contains(claude, want) {
			t.Errorf("claude args %q lack %q", claude, want)
		}
	}
	codex := strings.Join(Args(Target{Runner: "codex", Model: "codex-small", Effort: "low"}), " ")
	for _, want := range []string{"exec --json --ephemeral", "--sandbox read-only", "-m codex-small", "model_reasoning_effort=low"} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex args %q lack %q", codex, want)
		}
	}
}

func TestUsageEventSkipsTokenlessTries(t *testing.T) {
	tg := Target{Provider: "chatgpt", Runner: "codex", Model: "codex-small", Effort: "low"}
	if _, ok := UsageEvent(tg, "local", 1, Attempt{}, at); ok {
		t.Error("a try with no tokens writes no line")
	}
	ev, ok := UsageEvent(tg, "local", 2, Attempt{Session: "th", Tokens: usagehook.Tokens{Input: 5, Output: 1}}, at)
	if !ok || ev.Runner != "codex" || ev.Instance != "models-probe" || ev.Model != "codex-small" || ev.Effort != "low" || ev.TurnID != "probe:chatgpt:local:1795521600:2" || ev.Session != "th" {
		t.Errorf("event = %+v", ev)
	}
}
