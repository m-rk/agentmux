// Package probe sends each model provider tank one tiny request ("reply
// OK") through its runner's headless mode, on its cheapest model, and
// keeps the result as catalogue.ProviderHealth. It never launches an
// instance or touches a session: each try is one short-lived runner
// process in an empty temporary directory. See docs/models-probe.md.
package probe

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/usagehook"
)

// Prompt is the whole request.
const Prompt = "Reply with the single word OK."

// ClaudeModel is the model a Claude probe asks for: the CLI's alias for
// the current Haiku, so a new Haiku needs no change here.
const ClaudeModel = "haiku"

// Tries is how many times a provider is asked before it counts as down.
const Tries = 3

// DefaultBackoff is the wait before the second and third try.
var DefaultBackoff = []time.Duration{5 * time.Second, 15 * time.Second}

// TryTimeout bounds one try.
const TryTimeout = 90 * time.Second

// Target is one provider tank and how to ask it.
type Target struct {
	Provider string
	Label    string
	Runner   string
	// Model is the model to pass (-m/--model); for amp it is the mode.
	Model  string
	Effort string
	// Instance is an opencode or kilo instance whose provider config the
	// probe borrows (read only); the provider is defined per instance.
	Instance string
}

// Failure is why a try failed, as a kind and fixed plain words.
type Failure struct {
	Kind  string
	Words string
}

// Attempt is one try.
type Attempt struct {
	Latency time.Duration
	Tokens  usagehook.Tokens
	// Session is the runner's own thread or session id, if it said; only
	// for the usage line.
	Session string
	// Err is nil when the provider answered OK.
	Err *Failure
	// threadGone is set when amp refused to continue the standing
	// thread because it no longer exists or is archived.
	threadGone bool
}

// TryFunc asks a target once.
type TryFunc func(ctx context.Context, t Target) Attempt

// Options are the probe's moving parts, fakes in tests.
type Options struct {
	Try     TryFunc
	Sleep   func(context.Context, time.Duration)
	Now     func() time.Time
	Backoff []time.Duration
	// Logged is called after every try, for the usage file.
	Logged func(t Target, try int, a Attempt)
}

var tierRank = map[string]int{catalogue.TierEasy: 0, catalogue.TierHard: 1, catalogue.TierHardest: 2}

// Targets lists the provider tanks the catalogue configures on host, each
// on its cheapest entry: the lowest tier, then the first in the file.
// Claude always asks Haiku and Codex its lowest effort.
func Targets(cfg catalogue.Config, host string) []Target {
	offs := make([]catalogue.Offering, 0, len(cfg.Offerings))
	for _, o := range cfg.Offerings {
		for _, h := range o.Hosts {
			if h == host {
				offs = append(offs, o)
				break
			}
		}
	}
	sort.SliceStable(offs, func(i, j int) bool { return tierRank[offs[i].Tier] < tierRank[offs[j].Tier] })
	seen := map[string]bool{}
	var out []Target
	for _, o := range offs {
		provider, label := ProviderOf(o)
		if provider == "" {
			continue
		}
		t := Target{Provider: provider, Label: label, Runner: o.Runner, Model: o.Model}
		switch o.Runner {
		case catalogue.RunnerClaudeCode:
			t.Model = ClaudeModel
		case catalogue.RunnerCodex:
			t.Effort = "low"
			if len(o.Efforts) > 0 {
				t.Effort = o.Efforts[0]
			}
		case catalogue.RunnerAmp:
			t.Model = o.Mode
		case catalogue.RunnerOpencode, catalogue.RunnerKilo:
			if len(o.Instances) > 0 {
				t.Instance = o.Instances[0]
			}
		}
		if seen[t.Provider] {
			continue
		}
		seen[t.Provider] = true
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// ProviderOf is the provider tank an offering draws on and its label, or
// "" for an offering no probe covers.
func ProviderOf(o catalogue.Offering) (provider, label string) {
	switch o.Runner {
	case catalogue.RunnerClaudeCode:
		return "claude", "Claude"
	case catalogue.RunnerCodex:
		return "chatgpt", "ChatGPT"
	case catalogue.RunnerAmp:
		return "amp", "Amp"
	case catalogue.RunnerOpencode, catalogue.RunnerKilo:
		if p, _, ok := strings.Cut(o.Model, "/"); ok && p != "" {
			return p, p
		}
	}
	return "", ""
}

// One asks t up to Tries times and returns the last try and how many it
// took. A pass on any try is a pass.
func One(ctx context.Context, o Options, t Target) (Attempt, int) {
	var a Attempt
	for n := 1; n <= Tries; n++ {
		if n > 1 {
			if wait := backoff(o.Backoff, n-2); wait > 0 && o.Sleep != nil {
				o.Sleep(ctx, wait)
			}
		}
		if ctx.Err() != nil {
			return Attempt{Err: &Failure{Kind: catalogue.ErrKindTimeout, Words: "probe was cancelled"}}, n - 1
		}
		a = o.Try(ctx, t)
		if o.Logged != nil {
			o.Logged(t, n, a)
		}
		if a.Err == nil {
			return a, n
		}
	}
	return a, Tries
}

func backoff(b []time.Duration, i int) time.Duration {
	if len(b) == 0 {
		return 0
	}
	if i >= len(b) {
		return b[len(b)-1]
	}
	return b[i]
}

// Record folds a probe of t into its previous row (zero if none).
func Record(prev catalogue.ProviderHealth, t Target, host, hostLabel string, a Attempt, tries int, at time.Time) catalogue.ProviderHealth {
	ts := at.UTC().Format(time.RFC3339)
	h := catalogue.ProviderHealth{
		Provider: t.Provider, Label: t.Label, Runner: t.Runner, Model: t.Model,
		Host: host, HostLabel: hostLabel,
		CheckedAt: ts, LastOKAt: prev.LastOKAt,
		LatencyMS: a.Latency.Milliseconds(), Attempts: tries,
	}
	if a.Err == nil {
		h.Status, h.LastOKAt = catalogue.HealthOK, ts
		return h
	}
	h.Status = catalogue.HealthDown
	h.ErrorKind, h.Error = a.Err.Kind, a.Err.Words
	h.DownSince = ts
	if prev.Status == catalogue.HealthDown && prev.DownSince != "" {
		h.DownSince = prev.DownSince
	}
	return h
}

// Run probes every target in parallel and returns the new rows for host,
// built on prev (the state's rows, any host).
func Run(ctx context.Context, o Options, targets []Target, host, hostLabel string, prev []catalogue.ProviderHealth) []catalogue.ProviderHealth {
	old := map[string]catalogue.ProviderHealth{}
	for _, p := range prev {
		old[p.Key()] = p
	}
	out := make([]catalogue.ProviderHealth, len(targets))
	done := make(chan struct{}, len(targets))
	for i, t := range targets {
		go func(i int, t Target) {
			defer func() { done <- struct{}{} }()
			a, n := One(ctx, o, t)
			key := catalogue.ProviderHealth{Provider: t.Provider, Host: host}.Key()
			out[i] = Record(old[key], t, host, hostLabel, a, n, o.Now())
		}(i, t)
	}
	for range targets {
		<-done
	}
	return out
}

// Merge replaces the rows of host in state with rows, keeping other hosts'.
func Merge(prev []catalogue.ProviderHealth, host string, rows []catalogue.ProviderHealth) []catalogue.ProviderHealth {
	out := make([]catalogue.ProviderHealth, 0, len(prev)+len(rows))
	for _, p := range prev {
		if p.Host != host {
			out = append(out, p)
		}
	}
	return append(out, rows...)
}

// Rows is the "providers" list of `models list -json`: one row for each
// provider tank the catalogue configures on each host, from that host's
// report. A host that could not be checked, or has not probed, still gets
// its rows, as unknown with the reason.
func Rows(cfg catalogue.Config, reports map[string]catalogue.HostReport, localName string) []catalogue.ProviderHealth {
	out := []catalogue.ProviderHealth{}
	for _, host := range cfg.Hosts() {
		label := host
		if host == "local" && localName != "" {
			label = localName
		}
		rep, checked := reports[host]
		own := map[string]catalogue.ProviderHealth{}
		for _, p := range rep.Probes {
			if p.Host == "local" {
				own[p.Provider] = p
			}
		}
		for _, t := range Targets(cfg, host) {
			row, ok := own[t.Provider]
			if !ok {
				row = catalogue.ProviderHealth{Provider: t.Provider, Label: t.Label, Runner: t.Runner, Model: t.Model,
					Status: catalogue.HealthUnknown, ErrorKind: catalogue.ErrKindOther}
				switch {
				case !checked:
					row.Error = "host was not checked"
				case rep.Err != "":
					row.Error = "host " + rep.Err
				case rep.Probes == nil:
					row.Error = "host does not report probes yet"
				default:
					row.Error = "not probed yet"
				}
			}
			row.Host, row.HostLabel = host, label
			out = append(out, row)
		}
	}
	return out
}
