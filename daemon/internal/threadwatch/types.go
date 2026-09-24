// Package threadwatch follows each instance's agent turns, raises
// intervene alerts to Discord, and logs insights for the nightly review.
// See docs/design/thread-watch.md.
//
// This file is the shared contract between collectors, the event log,
// detectors, alerting, and the Jev gates. Change it deliberately.
package threadwatch

import (
	"context"
	"time"
)

// Event kinds. Collectors emit these; detectors consume them.
const (
	KindTurnEnd      = "turn_end"      // a turn finished; Duration/Tokens set when known
	KindAPIError     = "api_error"     // model/provider API error surfaced to the agent
	KindToolError    = "tool_error"    // a tool call returned an error
	KindAuthError    = "auth_error"    // login/session expired, 401
	KindCompaction   = "compaction"    // context compacted
	KindUserMessage  = "user_message"  // the human (or a remote client) sent a message
	KindAssistantMsg = "assistant_msg" // final assistant text of a turn; Excerpt holds its tail
	KindActivity     = "activity"      // any other transcript progress (tool use, partial output)
	KindStatus       = "status"        // discovery status change; Status set
	KindSessionExit  = "session_exit"  // session/process ended
	KindUsageLimit   = "usage_limit"   // a usage/session/credit/rate limit interrupted a turn
)

// Signal tiers.
const (
	TierIntervene = "intervene"
	TierInsight   = "insight"
)

// Signal codes.
const (
	CodeAwaitingUser    = "awaiting_user"
	CodeErrorLoop       = "error_loop"
	CodeAuthFailed      = "auth_failed"
	CodeStalledTurn     = "stalled_turn"
	CodeDiedMidTurn     = "died_mid_turn"
	CodeSlowTurn        = "slow_turn"
	CodeExpensiveTurn   = "expensive_turn"
	CodeRecoveredErrors = "recovered_errors"
	CodeCompactionChurn = "compaction_churn"
	CodeRetryThrash     = "retry_thrash"
	CodeLongWait        = "long_wait"
	CodeUsageLimit      = "usage_limit"
)

// MaxExcerptBytes caps Event.Excerpt and Signal.Evidence.
const MaxExcerptBytes = 2048

type Usage struct {
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

// Event is one normalised observation from an agent's own records.
type Event struct {
	Time     time.Time     `json:"time"`
	Instance string        `json:"instance"`
	Agent    string        `json:"agent"`            // claude-code, amp, opencode, ...
	Thread   string        `json:"thread,omitempty"` // Claude session id, amp threadId, opencode session id
	Kind     string        `json:"kind"`
	Duration time.Duration `json:"duration_ns,omitempty"`
	Tokens   Usage         `json:"tokens,omitempty"`
	Tool     string        `json:"tool,omitempty"`    // tool name for tool_error/activity
	Status   string        `json:"status,omitempty"`  // for KindStatus: running, idle, stopped
	Excerpt  string        `json:"excerpt,omitempty"` // capped, redacted
}

// Signal is a detector's conclusion about an instance/thread.
type Signal struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
	Thread   string    `json:"thread,omitempty"`
	Code     string    `json:"code"`
	Tier     string    `json:"tier"`
	Reason   string    `json:"reason"`             // one line, human readable
	Evidence string    `json:"evidence,omitempty"` // capped excerpt
	Resolved bool      `json:"resolved,omitempty"` // condition cleared
	// Judgment holds Jev's verdict when one was requested (nil otherwise).
	Judgment *Judgment `json:"judgment,omitempty"`
}

// Judgment is the typed result of the Jev gates for one signal/event.
type Judgment struct {
	WaitingKind      string             `json:"waiting_kind,omitempty"`
	WaitingKindProbs map[string]float64 `json:"waiting_kind_probs,omitempty"`
	NeedsHumanNow    float64            `json:"needs_human_now,omitempty"` // Noul probability
	Urgency          float64            `json:"urgency,omitempty"`         // Score, 1..5
	UrgencyConf      float64            `json:"urgency_confidence,omitempty"`
	LikelyTransient  float64            `json:"likely_transient,omitempty"` // Noul probability
	Category         string             `json:"category,omitempty"`
	FixableByConfig  float64            `json:"fixable_by_config,omitempty"` // Noul probability
	Err              string             `json:"error,omitempty"`             // set when the call failed
}

// Instance is what a collector needs to know about an agentmux instance.
type Instance struct {
	Name    string
	Agent   string
	Workdir string
	Home    string // run user's home directory
	Status  string // running, idle, stopped
}

// Collector reads one agent's records for one instance and returns new
// events since the last call. Offsets are persisted by the collector via
// the OffsetStore so restarts resume without re-emitting.
type Collector interface {
	Poll(ctx context.Context, inst Instance, offsets OffsetStore) ([]Event, error)
}

// OffsetStore persists per-source read positions (byte offset, row id, ...).
type OffsetStore interface {
	Get(key string) (int64, bool)
	Set(key string, value int64)
}

// Judge asks Jev about an intervene candidate. Implementations must never
// return an error that suppresses an alert: on failure they return a
// Judgment with Err set.
type Judge interface {
	Judge(ctx context.Context, sig Signal, recent []Event) Judgment
}
