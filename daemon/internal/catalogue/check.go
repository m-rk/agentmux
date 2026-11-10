package catalogue

import (
	"os/exec"
	"sort"
	"time"
)

// runnerBinaries maps a runner to the executable that launches it.
var runnerBinaries = map[string]string{
	RunnerClaudeCode: "claude",
	RunnerCodex:      "codex",
	RunnerAmp:        "amp",
	RunnerOpencode:   "opencode",
	RunnerKilo:       "kilo",
}

// LocalRunners reports which runners are installed on this machine, by
// looking for their executable on PATH. It runs nothing.
func LocalRunners() map[string]bool {
	out := map[string]bool{}
	for r, bin := range runnerBinaries {
		_, err := exec.LookPath(bin)
		out[r] = err == nil
	}
	return out
}

// HostReport is what a check of one host found. Err is set when the
// check itself failed (host unreachable, gateway too old, access denied);
// Runners is then empty.
type HostReport struct {
	Runners   map[string]bool
	Err       string
	CheckedAt time.Time
}

// Hosts lists every host the config names, sorted.
func (c Config) Hosts() []string {
	set := map[string]bool{}
	for _, o := range c.Offerings {
		for _, h := range o.Hosts {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Build joins the config with the per-host reports into the catalogue a
// caller reads. Every configured host of every offering gets a result: a
// host with no report, or whose check failed, is unavailable with the
// reason, never omitted.
func Build(c Config, reports map[string]HostReport, localName string) Catalogue {
	cat := Catalogue{Version: FormatVersion, Fuel: c.Fuel, Offerings: make([]Offering, 0, len(c.Offerings))}
	for _, o := range c.Offerings {
		if o.Icon == "" {
			o.Icon = IconKey(o.Runner)
		}
		o.Availability = nil
		for _, h := range o.Hosts {
			a := HostAvailability{Host: h, Label: h}
			if h == "local" && localName != "" {
				a.Label = localName
			}
			rep, ok := reports[h]
			switch {
			case !ok:
				a.Reason = "host was not checked"
			case rep.Err != "":
				a.Reason = rep.Err
			case !rep.Runners[o.Runner]:
				a.Reason = o.Runner + " is not installed"
			default:
				a.Available = true
			}
			if ok && !rep.CheckedAt.IsZero() {
				a.CheckedAt = rep.CheckedAt.UTC().Format(time.RFC3339)
			}
			o.Availability = append(o.Availability, a)
		}
		cat.Offerings = append(cat.Offerings, o)
	}
	return cat
}
