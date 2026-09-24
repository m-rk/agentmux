package threadwatch

import (
	"regexp"
	"strings"
	"time"
)

// Config is the shape of ~/.config/agentmux/threadwatch.yaml. Zero values
// mean "use the default" (see DefaultConfig). Loading lives in store.go.
type Config struct {
	Thresholds Thresholds              `yaml:"thresholds"`
	Alerts     AlertConfig             `yaml:"alerts"`
	Jev        JevConfig               `yaml:"jev"`
	Instances  map[string]InstanceConf `yaml:"instances,omitempty"`
}

type Thresholds struct {
	AwaitingUserAfter  time.Duration `yaml:"awaiting_user_after"`  // default 10m
	StalledTurnAfter   time.Duration `yaml:"stalled_turn_after"`   // default 15m
	APIErrorLoop       int           `yaml:"api_error_loop"`       // default 3 consecutive
	ToolErrorLoop      int           `yaml:"tool_error_loop"`      // default 4 same tool error in one turn
	SlowTurnMin        time.Duration `yaml:"slow_turn_min"`        // default 5m (and > instance p90)
	CompactionsPerDay  int           `yaml:"compactions_per_day"`  // default 2 per thread
	LongWaitAfter      time.Duration `yaml:"long_wait_after"`      // default 2h
	RetryThrashRepeats int           `yaml:"retry_thrash_repeats"` // default 3
}

type AlertConfig struct {
	Cooldown      time.Duration `yaml:"cooldown"`        // default 1h per (instance, thread, code)
	MaxPerHour    int           `yaml:"max_per_hour"`    // default 6 per host
	ResolvedAfter time.Duration `yaml:"resolved_window"` // default 1h: send "resolved" only within this
}

type JevConfig struct {
	Mode            string  `yaml:"mode"`              // off | shadow | live (default shadow when a key is present)
	Model           string  `yaml:"model"`             // default jev-latest
	PageUrgency     float64 `yaml:"page_urgency"`      // default 4 (Score 1..5)
	PageConfidence  float64 `yaml:"page_confidence"`   // default 0.6
	AwaitingMinProb float64 `yaml:"awaiting_min_prob"` // default 0.5
}

// InstanceConf overrides per instance.
type InstanceConf struct {
	Disabled   bool        `yaml:"disabled,omitempty"`
	Jev        *bool       `yaml:"jev,omitempty"`    // false: never send excerpts to TypeSafe
	Review     *bool       `yaml:"review,omitempty"` // false: exclude from nightly Claude review
	Thresholds *Thresholds `yaml:"thresholds,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Thresholds: Thresholds{
			AwaitingUserAfter:  10 * time.Minute,
			StalledTurnAfter:   15 * time.Minute,
			APIErrorLoop:       3,
			ToolErrorLoop:      4,
			SlowTurnMin:        5 * time.Minute,
			CompactionsPerDay:  2,
			LongWaitAfter:      2 * time.Hour,
			RetryThrashRepeats: 3,
		},
		Alerts: AlertConfig{Cooldown: time.Hour, MaxPerHour: 6, ResolvedAfter: time.Hour},
		Jev:    JevConfig{Mode: "shadow", Model: "jev-latest", PageUrgency: 4, PageConfidence: 0.6, AwaitingMinProb: 0.5},
	}
}

// ThresholdsFor returns the effective thresholds for an instance.
func (c Config) ThresholdsFor(instance string) Thresholds {
	t := c.Thresholds
	if ic, ok := c.Instances[instance]; ok && ic.Thresholds != nil {
		o := *ic.Thresholds
		if o.AwaitingUserAfter > 0 {
			t.AwaitingUserAfter = o.AwaitingUserAfter
		}
		if o.StalledTurnAfter > 0 {
			t.StalledTurnAfter = o.StalledTurnAfter
		}
		if o.APIErrorLoop > 0 {
			t.APIErrorLoop = o.APIErrorLoop
		}
		if o.ToolErrorLoop > 0 {
			t.ToolErrorLoop = o.ToolErrorLoop
		}
		if o.SlowTurnMin > 0 {
			t.SlowTurnMin = o.SlowTurnMin
		}
		if o.CompactionsPerDay > 0 {
			t.CompactionsPerDay = o.CompactionsPerDay
		}
		if o.LongWaitAfter > 0 {
			t.LongWaitAfter = o.LongWaitAfter
		}
		if o.RetryThrashRepeats > 0 {
			t.RetryThrashRepeats = o.RetryThrashRepeats
		}
	}
	return t
}

// JevAllowed reports whether excerpts from instance may be sent to TypeSafe.
func (c Config) JevAllowed(instance string) bool {
	if c.Jev.Mode == "off" {
		return false
	}
	if ic, ok := c.Instances[instance]; ok && ic.Jev != nil {
		return *ic.Jev
	}
	return true
}

var secretPatterns = regexp.MustCompile(`(?i)(sk-ant-[a-z0-9_-]+|sgamp_[a-z0-9_]+|ops_[a-z0-9_-]{20,}|gh[pousr]_[a-z0-9]{20,}|xox[abpr]-[a-z0-9-]+|AKIA[0-9A-Z]{16}|eyJ[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\.[a-z0-9_-]+|(api[_-]?key|token|secret|password|bearer)(["'\s:=]+)[^\s"']{8,})`)

// Redact masks likely secrets in s.
func Redact(s string) string {
	return secretPatterns.ReplaceAllStringFunc(s, func(m string) string {
		if sub := secretPatterns.FindStringSubmatch(m); len(sub) > 3 && sub[2] != "" {
			return sub[2] + sub[3] + "[redacted]"
		}
		return "[redacted]"
	})
}

// Excerpt redacts s and keeps its last MaxExcerptBytes bytes (tails matter
// most for "what is the agent waiting on"), on a UTF-8 boundary.
func Excerpt(s string) string {
	s = Redact(strings.TrimSpace(s))
	if len(s) <= MaxExcerptBytes {
		return s
	}
	cut := len(s) - MaxExcerptBytes
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return "…" + s[cut:]
}
