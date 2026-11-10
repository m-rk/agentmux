// Package catalogue is the format of the model catalogue: one entry
// (an Offering) for each thing a runner can be launched as. agentmux owns
// the format and the checks; the real entries live in host config
// (~/.config/agentmux/catalogue.yaml), never in the repo.
//
// The JSON shape of Offering and Catalogue is a contract for other
// programs (dispatch option builders, UIs). Add fields; don't rename or
// repurpose them.
package catalogue

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FormatVersion is bumped only for a change that breaks readers.
const FormatVersion = 1

// Runner names: what an instance's agent is called elsewhere in agentmux.
const (
	RunnerClaudeCode = "claude-code"
	RunnerCodex      = "codex"
	RunnerAmp        = "amp"
	RunnerOpencode   = "opencode"
	RunnerKilo       = "kilo"
)

// Fuel tanks: which pool of usage an offering draws from.
const (
	FuelClaude   = "claude"
	FuelChatGPT  = "chatgpt"
	FuelAmp      = "amp"
	FuelProvider = "provider"
)

// Tiers: how hard a job the offering is meant for.
const (
	TierEasy    = "easy"
	TierHard    = "hard"
	TierHardest = "hardest"
)

var runnerIcons = map[string]string{
	RunnerClaudeCode: "claude",
	RunnerCodex:      "codex",
	RunnerAmp:        "amp",
	RunnerOpencode:   "opencode",
	RunnerKilo:       "kilo",
}

// IconKey is the stable key a UI maps to a runner's icon. It is a
// property of the runner, not of config, so it never drifts per entry.
// An unknown runner has no icon key.
func IconKey(runner string) string { return runnerIcons[runner] }

// Runners lists the known runner names, sorted.
func Runners() []string {
	out := make([]string, 0, len(runnerIcons))
	for r := range runnerIcons {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Offering is one launchable thing.
type Offering struct {
	// ID is the entry's stable key, unique in the catalogue. It is what
	// callers store and pass back; labels may change, IDs don't.
	ID string `yaml:"id" json:"id"`
	// Runner is one of the Runner* names.
	Runner string `yaml:"runner" json:"runner"`
	// Icon is IconKey(Runner); filled in on load, ignored in config.
	Icon string `yaml:"-" json:"icon"`
	// Model is the model id the runner is given. For Amp, the mode's
	// own model is not known to agentmux, so Model may be empty and
	// Mode is the selector.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Version is the model's version, as a display string.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Label is the short name to show.
	Label string `yaml:"label" json:"label"`
	// Family groups related offerings, e.g. all sizes of one model line.
	Family string `yaml:"family,omitempty" json:"family,omitempty"`
	// Efforts are the effort or reasoning levels it accepts, lowest
	// first. Empty: the offering has no effort setting.
	Efforts []string `yaml:"efforts,omitempty" json:"efforts,omitempty"`
	// Speeds are the serving speeds it accepts, e.g. "standard" and
	// "fast". Empty: no speed option.
	Speeds []string `yaml:"speeds,omitempty" json:"speeds,omitempty"`
	// Fuel is the usage pool it draws from: one of the Fuel* names.
	Fuel string `yaml:"fuel" json:"fuel"`
	// Tier is the difficulty tier: easy, hard or hardest.
	Tier string `yaml:"tier" json:"tier"`
	// Mode is the Amp mode (a built-in or a plugin mode) a launch passes
	// with -m. Only for runner "amp"; each mode is its own offering, so a
	// launch picks one instead of the host-wide amp.yaml mode.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Instances names existing instances that carry this offering. For
	// opencode and kilo, whose provider and model are fixed when an
	// instance is created, this is how a caller finds one that has it.
	Instances []string `yaml:"instances,omitempty" json:"instances,omitempty"`
	// Hosts are the hosts the entry is configured for, by name as in
	// hosts.yaml ("local" for the machine running the command).
	Hosts []string `yaml:"hosts" json:"-"`
	// Availability is the live per-host check; filled by Check, never
	// read from config.
	Availability []HostAvailability `yaml:"-" json:"hosts"`
}

// HostAvailability is the result of checking one offering on one host.
// A check that failed or could not run is reported with Available false
// and a Reason; the entry is never dropped.
type HostAvailability struct {
	Host      string `json:"host"`
	Available bool   `json:"available"`
	// Reason says why not, when Available is false.
	Reason string `json:"reason,omitempty"`
	// CheckedAt is when the check ran (RFC 3339). Empty if it never did.
	CheckedAt string `json:"checked_at,omitempty"`
}

// Fuel describes one fuel tank. The cap on a tank is a fact the
// operator sets by hand when it cannot be read from the provider.
type FuelTank struct {
	Name string `yaml:"name" json:"name"`
	// CapPercent is the share of the tank agentmux-launched work may
	// use, 0-100. Zero: no cap recorded.
	CapPercent int `yaml:"cap_percent,omitempty" json:"cap_percent,omitempty"`
	// CapSource is "manual" (set by hand in config) or "provider"
	// (read from the provider).
	CapSource string `yaml:"cap_source,omitempty" json:"cap_source,omitempty"`
	Note      string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Config is the host config file.
type Config struct {
	Offerings []Offering `yaml:"offerings"`
	Fuel      []FuelTank `yaml:"fuel,omitempty"`
}

// Catalogue is what `agentmux models list -json` carries under the
// "catalogue" key.
type Catalogue struct {
	Version   int        `json:"version"`
	Offerings []Offering `json:"offerings"`
	Fuel      []FuelTank `json:"fuel,omitempty"`
}

// DefaultPath is ~/.config/agentmux/catalogue.yaml.
func DefaultPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".config", "agentmux", "catalogue.yaml")
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Load reads and validates the catalogue config. A missing file is an
// empty catalogue.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return Parse(b)
}

// Parse validates catalogue config bytes.
func Parse(b []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("catalogue: %w", err)
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	for i := range c.Offerings {
		c.Offerings[i].Icon = IconKey(c.Offerings[i].Runner)
	}
	return c, nil
}

// Validate checks every entry and names the entry and field at fault.
func (c Config) Validate() error {
	seen := map[string]bool{}
	for i, o := range c.Offerings {
		who := fmt.Sprintf("offering %d", i+1)
		if o.ID != "" {
			who = fmt.Sprintf("offering %q", o.ID)
		}
		if !idPattern.MatchString(o.ID) {
			return fmt.Errorf("catalogue: %s: id must be lowercase letters, digits, '.', '_' or '-'", who)
		}
		if seen[o.ID] {
			return fmt.Errorf("catalogue: %s: duplicate id", who)
		}
		seen[o.ID] = true
		if IconKey(o.Runner) == "" {
			return fmt.Errorf("catalogue: %s: unknown runner %q (want one of %s)", who, o.Runner, strings.Join(Runners(), ", "))
		}
		if strings.TrimSpace(o.Label) == "" {
			return fmt.Errorf("catalogue: %s: label is required", who)
		}
		switch o.Fuel {
		case FuelClaude, FuelChatGPT, FuelAmp, FuelProvider:
		default:
			return fmt.Errorf("catalogue: %s: unknown fuel %q", who, o.Fuel)
		}
		switch o.Tier {
		case TierEasy, TierHard, TierHardest:
		default:
			return fmt.Errorf("catalogue: %s: tier must be easy, hard or hardest", who)
		}
		if o.Runner == RunnerAmp {
			if strings.TrimSpace(o.Mode) == "" {
				return fmt.Errorf("catalogue: %s: an amp offering needs a mode", who)
			}
		} else if o.Mode != "" {
			return fmt.Errorf("catalogue: %s: mode is only for the amp runner", who)
		} else if strings.TrimSpace(o.Model) == "" {
			return fmt.Errorf("catalogue: %s: model is required", who)
		}
		if len(o.Hosts) == 0 {
			return fmt.Errorf("catalogue: %s: hosts is required", who)
		}
	}
	for _, f := range c.Fuel {
		if f.CapPercent < 0 || f.CapPercent > 100 {
			return fmt.Errorf("catalogue: fuel %q: cap_percent must be 0-100", f.Name)
		}
		switch f.CapSource {
		case "", "manual", "provider":
		default:
			return fmt.Errorf("catalogue: fuel %q: cap_source must be manual or provider", f.Name)
		}
	}
	return nil
}
