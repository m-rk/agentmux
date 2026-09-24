// Config loading and event/signal persistence for thread watch. See
// docs/design/thread-watch.md and types.go/config.go for the contract this
// builds on.
package threadwatch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath returns ~/.config/agentmux/threadwatch.yaml for home.
func DefaultConfigPath(home string) string {
	return filepath.Join(home, ".config", "agentmux", "threadwatch.yaml")
}

// StateDir returns ~/.local/state/agentmux/threadwatch for home: the
// directory event/signal logs and offsets live in.
func StateDir(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "threadwatch")
}

// ReviewDir returns ~/.local/state/agentmux/reviews for home: where the
// nightly review writes its full report.
func ReviewDir(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "reviews")
}

// LoadConfig reads and parses the threadwatch.yaml at path, applying it on
// top of DefaultConfig() so fields left unset in the file keep their
// defaults. A missing file is not an error: it returns DefaultConfig().
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var y yamlConfig
	if err := yaml.Unmarshal(data, &y); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	mergeThresholds(&cfg.Thresholds, y.Thresholds)
	mergeAlerts(&cfg.Alerts, y.Alerts)
	mergeJev(&cfg.Jev, y.Jev)
	if len(y.Instances) > 0 {
		cfg.Instances = make(map[string]InstanceConf, len(y.Instances))
		for name, ic := range y.Instances {
			cfg.Instances[name] = ic.toInstanceConf()
		}
	}

	switch cfg.Jev.Mode {
	case "off", "shadow", "live":
	default:
		return Config{}, fmt.Errorf("%s: invalid jev.mode %q (want off, shadow, or live)", path, cfg.Jev.Mode)
	}
	checkAPIKey(&cfg.Jev, path)

	return cfg, nil
}

// checkAPIKey drops an unusable TypeSafe key setting and records why in
// KeyProblem, rather than failing the load: Jev is optional, and a key
// mistake must not stop deterministic alerting. A literal key is only
// accepted from a file no one else can read.
func checkAPIKey(jc *JevConfig, path string) {
	switch {
	case jc.APIKey != "" && jc.APIKeyRef != "":
		jc.KeyProblem = "set jev.api_key or jev.api_key_ref, not both"
	case jc.APIKeyRef != "" && !validOpRef(jc.APIKeyRef):
		jc.KeyProblem = "jev.api_key_ref must look like op://<vault-id>/<item-id>/<field>"
	case jc.APIKey != "":
		info, err := os.Stat(path)
		if err != nil {
			jc.KeyProblem = fmt.Sprintf("checking %s: %v", path, err)
		} else if perm := info.Mode().Perm(); perm&0o077 != 0 {
			jc.KeyProblem = fmt.Sprintf("jev.api_key ignored: %s is mode %03o; run chmod 600 %s", path, perm, path)
		}
	}
	if jc.KeyProblem != "" {
		jc.APIKey, jc.APIKeyRef = "", ""
	}
}

// validOpRef accepts op://vault/item/field and op://vault/item/section/field.
func validOpRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, "op://")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// yamlConfig mirrors Config for YAML decoding, with pointer fields so
// LoadConfig can tell "absent from the file" apart from "zero value" and
// leave absent fields at their DefaultConfig() value. Duration fields use
// yamlDuration, which accepts both a Go duration string ("10m", "2h") and a
// bare integer number of seconds, since yaml.v3 has no native support for
// decoding either into time.Duration.
type yamlConfig struct {
	Thresholds *yamlThresholds             `yaml:"thresholds"`
	Alerts     *yamlAlertConfig            `yaml:"alerts"`
	Jev        *yamlJevConfig              `yaml:"jev"`
	Instances  map[string]yamlInstanceConf `yaml:"instances"`
}

type yamlThresholds struct {
	AwaitingUserAfter  *yamlDuration `yaml:"awaiting_user_after"`
	StalledTurnAfter   *yamlDuration `yaml:"stalled_turn_after"`
	APIErrorLoop       *int          `yaml:"api_error_loop"`
	ToolErrorLoop      *int          `yaml:"tool_error_loop"`
	SlowTurnMin        *yamlDuration `yaml:"slow_turn_min"`
	CompactionsPerDay  *int          `yaml:"compactions_per_day"`
	LongWaitAfter      *yamlDuration `yaml:"long_wait_after"`
	RetryThrashRepeats *int          `yaml:"retry_thrash_repeats"`
}

type yamlAlertConfig struct {
	Cooldown      *yamlDuration `yaml:"cooldown"`
	MaxPerHour    *int          `yaml:"max_per_hour"`
	ResolvedAfter *yamlDuration `yaml:"resolved_window"`
}

type yamlJevConfig struct {
	Mode            *string  `yaml:"mode"`
	Model           *string  `yaml:"model"`
	PageUrgency     *float64 `yaml:"page_urgency"`
	PageConfidence  *float64 `yaml:"page_confidence"`
	AwaitingMinProb *float64 `yaml:"awaiting_min_prob"`
	APIKey          *string  `yaml:"api_key"`
	APIKeyRef       *string  `yaml:"api_key_ref"`
}

type yamlInstanceConf struct {
	Disabled   *bool           `yaml:"disabled"`
	Jev        *bool           `yaml:"jev"`
	Review     *bool           `yaml:"review"`
	Thresholds *yamlThresholds `yaml:"thresholds"`
}

func (y yamlInstanceConf) toInstanceConf() InstanceConf {
	var ic InstanceConf
	if y.Disabled != nil {
		ic.Disabled = *y.Disabled
	}
	ic.Jev = y.Jev
	ic.Review = y.Review
	if y.Thresholds != nil {
		// Instance-level Thresholds overrides start from the zero value:
		// ThresholdsFor only applies fields that are non-zero (see
		// config.go), so a field left unset here correctly means "no
		// override", not "default".
		var t Thresholds
		mergeThresholds(&t, y.Thresholds)
		ic.Thresholds = &t
	}
	return ic
}

func mergeThresholds(dst *Thresholds, src *yamlThresholds) {
	if src == nil {
		return
	}
	if src.AwaitingUserAfter != nil {
		dst.AwaitingUserAfter = time.Duration(*src.AwaitingUserAfter)
	}
	if src.StalledTurnAfter != nil {
		dst.StalledTurnAfter = time.Duration(*src.StalledTurnAfter)
	}
	if src.APIErrorLoop != nil {
		dst.APIErrorLoop = *src.APIErrorLoop
	}
	if src.ToolErrorLoop != nil {
		dst.ToolErrorLoop = *src.ToolErrorLoop
	}
	if src.SlowTurnMin != nil {
		dst.SlowTurnMin = time.Duration(*src.SlowTurnMin)
	}
	if src.CompactionsPerDay != nil {
		dst.CompactionsPerDay = *src.CompactionsPerDay
	}
	if src.LongWaitAfter != nil {
		dst.LongWaitAfter = time.Duration(*src.LongWaitAfter)
	}
	if src.RetryThrashRepeats != nil {
		dst.RetryThrashRepeats = *src.RetryThrashRepeats
	}
}

func mergeAlerts(dst *AlertConfig, src *yamlAlertConfig) {
	if src == nil {
		return
	}
	if src.Cooldown != nil {
		dst.Cooldown = time.Duration(*src.Cooldown)
	}
	if src.MaxPerHour != nil {
		dst.MaxPerHour = *src.MaxPerHour
	}
	if src.ResolvedAfter != nil {
		dst.ResolvedAfter = time.Duration(*src.ResolvedAfter)
	}
}

func mergeJev(dst *JevConfig, src *yamlJevConfig) {
	if src == nil {
		return
	}
	if src.Mode != nil {
		dst.Mode = *src.Mode
	}
	if src.Model != nil {
		dst.Model = *src.Model
	}
	if src.APIKey != nil {
		dst.APIKey = strings.TrimSpace(*src.APIKey)
	}
	if src.APIKeyRef != nil {
		dst.APIKeyRef = strings.TrimSpace(*src.APIKeyRef)
	}
	if src.PageUrgency != nil {
		dst.PageUrgency = *src.PageUrgency
	}
	if src.PageConfidence != nil {
		dst.PageConfidence = *src.PageConfidence
	}
	if src.AwaitingMinProb != nil {
		dst.AwaitingMinProb = *src.AwaitingMinProb
	}
}

// yamlDuration decodes a YAML duration field given either as a Go duration
// string ("10m", "2h30m") or a bare number, taken as a count of seconds.
type yamlDuration time.Duration

func (d *yamlDuration) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!str" {
		parsed, err := time.ParseDuration(value.Value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", value.Value, err)
		}
		*d = yamlDuration(parsed)
		return nil
	}
	var secs int64
	if err := value.Decode(&secs); err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	*d = yamlDuration(time.Duration(secs) * time.Second)
	return nil
}

// Store persists Events and Signals as newline-delimited JSON under a state
// directory, one file per UTC calendar day (events-YYYY-MM-DD.jsonl,
// signals-YYYY-MM-DD.jsonl). It is safe for concurrent use by goroutines
// within one writer process.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore returns a Store rooted at dir, creating it (mode 0700) if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// AppendEvents appends events to the day file matching each event's (UTC)
// Time, creating files and the state directory as needed.
func (s *Store) AppendEvents(events []Event) error {
	return appendJSONL(s, "events", events, func(e Event) time.Time { return e.Time })
}

// AppendSignals appends signals to the day file matching each signal's
// (UTC) Time, creating files and the state directory as needed.
func (s *Store) AppendSignals(signals []Signal) error {
	return appendJSONL(s, "signals", signals, func(sig Signal) time.Time { return sig.Time })
}

// ReadEvents returns events with Time in [since, until], read from every
// events day file that range touches. A corrupt or partial trailing line
// (e.g. from a write in progress) is skipped rather than failing the read.
func (s *Store) ReadEvents(since, until time.Time) ([]Event, error) {
	return readJSONL[Event](s, "events", since, until, func(e Event) time.Time { return e.Time })
}

// ReadSignals returns signals with Time in [since, until], read from every
// signals day file that range touches. A corrupt or partial trailing line
// is skipped rather than failing the read.
func (s *Store) ReadSignals(since, until time.Time) ([]Signal, error) {
	return readJSONL[Signal](s, "signals", since, until, func(sig Signal) time.Time { return sig.Time })
}

// Prune removes event/signal day files whose whole day is older than keep
// relative to now. keep <= 0 means the default of 14 days.
func (s *Store) Prune(now time.Time, keep time.Duration) error {
	if keep <= 0 {
		keep = 14 * 24 * time.Hour
	}
	cutoff := now.UTC().Add(-keep).Truncate(24 * time.Hour)

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		date, ok := dayFileDate(entry.Name())
		if !ok {
			continue
		}
		if date.Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

var dayFileRe = regexp.MustCompile(`^(?:events|signals)-(\d{4}-\d{2}-\d{2})\.jsonl$`)

func dayFileDate(name string) (time.Time, bool) {
	m := dayFileRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	d, err := time.Parse("2006-01-02", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

func appendJSONL[T any](s *Store, prefix string, items []T, timeOf func(T) time.Time) error {
	if len(items) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}

	var order []string
	byDate := map[string][]T{}
	for _, item := range items {
		date := timeOf(item).UTC().Format("2006-01-02")
		if _, ok := byDate[date]; !ok {
			order = append(order, date)
		}
		byDate[date] = append(byDate[date], item)
	}

	for _, date := range order {
		path := filepath.Join(s.dir, prefix+"-"+date+".jsonl")
		if err := appendJSONLFile(path, byDate[date]); err != nil {
			return err
		}
	}
	return nil
}

func appendJSONLFile[T any](path string, items []T) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readJSONL[T any](s *Store, prefix string, since, until time.Time, timeOf func(T) time.Time) ([]T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []T
	sinceUTC, untilUTC := since.UTC(), until.UTC()
	for d := sinceUTC.Truncate(24 * time.Hour); !d.After(untilUTC); d = d.AddDate(0, 0, 1) {
		path := filepath.Join(s.dir, prefix+"-"+d.Format("2006-01-02")+".jsonl")
		items, err := readJSONLFile[T](path)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			t := timeOf(item)
			if t.Before(sinceUTC) || t.After(untilUTC) {
				continue
			}
			out = append(out, item)
		}
	}
	return out, nil
}

func readJSONLFile[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []T
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var item T
		if err := json.Unmarshal(line, &item); err != nil {
			// Tolerate a corrupt or partial line (e.g. a concurrent write
			// still in flight, or a truncated last line after a crash).
			continue
		}
		out = append(out, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
