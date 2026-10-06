// Package ampconfig loads the host-local amp settings from
// ~/.config/agentmux/amp.yaml. The one required setting is mode, the amp
// agent mode (`-m/--mode`) every amp thread started by `sessions run` uses.
// Three optional display keys ride alongside it for buttons and status:
// label (full display name), short (short button text), and emoji (a list
// of emoji names, first provider, last model). The file is host-local on
// purpose: the mode names a model/provider choice that belongs to the
// machine, not the repo. Docs and tests use placeholders such as "high" —
// never put a real user's mode name in the repo.
//
// Every amp thread must carry -m (AMUX-36): callers resolve the mode with
// Require, which refuses when no mode is configured anywhere instead of
// falling back to amp's default model.
package ampconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvOverride is the per-instance registry value that overrides the host
// config: AGENTMUX_AMP_MODE, set by `agentmux new -amp-mode`.
const EnvOverride = "AGENTMUX_AMP_MODE"

// configFile is the file name under ~/.config/agentmux.
const configFile = "amp.yaml"

// Config is the parsed amp.yaml.
type Config struct {
	// Mode is the amp agent mode key or label passed to `amp -m`, e.g.
	// "high" or a plugin mode's key or label. Empty means no file or no
	// mode set: callers run amp without -m.
	Mode string
	// Label is the full display name for the mode, for buttons and
	// status. Empty means show Mode as-is.
	Label string
	// Short is the short button text for the mode. Empty means no short
	// text was configured.
	Short string
	// Emoji are the display emoji names for the mode (first provider,
	// last model). Empty means no emoji were configured.
	Emoji []string
}

// DefaultPath returns ~/.config/agentmux/amp.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", configFile)
}

// Load reads and parses the file at path. A missing file is not an error:
// it means no host default is configured. An empty mode is refused the
// same way an absent file is — "mode:" with nothing after it almost
// always means somebody commented out the value and left the key, and
// silently running amp without -m would hide that mistake.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var raw struct {
		Mode  string   `yaml:"mode"`
		Label string   `yaml:"label"`
		Short string   `yaml:"short"`
		Emoji []string `yaml:"emoji"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	mode := strings.TrimSpace(raw.Mode)
	if mode == "" {
		return Config{}, fmt.Errorf("parsing %s: mode is empty", path)
	}
	return Config{
		Mode:  mode,
		Label: strings.TrimSpace(raw.Label),
		Short: strings.TrimSpace(raw.Short),
		Emoji: cleanEmojiNames(raw.Emoji),
	}, nil
}

// cleanEmojiNames trims and drops blank emoji names, so `emoji: ["a", ""]`
// behaves like `emoji: ["a"]` and an all-blank list reads as unconfigured.
func cleanEmojiNames(names []string) []string {
	var out []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// DisplayOf resolves the display keys for an instance: the host config's
// label/short/emoji. An instance override changes only the -m value —
// display keys always come from the host file, and are empty when the
// host sets none.
func DisplayOf(host Config) (label, short string, emoji []string) {
	return host.Label, host.Short, host.Emoji
}

// Resolve is the effective mode for an instance: the instance's
// AGENTMUX_AMP_MODE registry value wins, otherwise the host config.
// It returns where the value came from for `sessions status -json`:
// "instance", "host", or "" when no mode is configured anywhere.
func Resolve(host Config, instanceMode string) (mode, source string) {
	if m := strings.TrimSpace(instanceMode); m != "" {
		return m, "instance"
	}
	if host.Mode != "" {
		return host.Mode, "host"
	}
	return "", ""
}

// ErrNoMode is the refusal when no amp mode is configured anywhere: no
// amp thread may start without -m, so running on amp's default model is
// never a silent fallback (AMUX-36). The message names both places a
// mode can come from, never a mode value.
var ErrNoMode = errors.New("no amp mode configured: set mode: in ~/.config/agentmux/amp.yaml or pass -amp-mode for the instance")

// Require is the effective mode for an amp spawn: Resolve, refused when
// nothing is configured. Every builder below funnels through it —
// sessions run and continue, the runner unit, the nightly review and
// doctor escalation, the mode probe — so no amp process starts without
// -m from any path. An explicit flag override (sessions run -mode,
// threadwatch.yaml review.amp.mode) bypasses Require by construction:
// the caller takes the flag when set, and only calls Require when it is
// empty.
func Require(host Config, instanceMode string) (mode, source string, err error) {
	mode, source = Resolve(host, instanceMode)
	if mode == "" {
		return "", "", ErrNoMode
	}
	return mode, source, nil
}

// SpawnArgs returns the -m pair every amp spawn must carry: ["-m",
// mode]. One builder for every amp command line (AMUX-36), so -m cannot
// be forgotten on one path while present on the others.
func SpawnArgs(mode string) []string {
	return []string{"-m", mode}
}
