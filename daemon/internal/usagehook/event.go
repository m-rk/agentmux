// Package usagehook records one line per finished agent turn, with its token
// counts, so a usage ledger (forgentic's usage table) can take them without
// reading transcripts or rollouts itself. The contract is in
// docs/completed-turn-usage.md.
package usagehook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Version is the payload version written as "v".
const Version = 1

// DefaultPath is the event file, relative to the run user's home.
const DefaultPath = ".agents/local/metrics/turn-completions.jsonl"

// PathEnv overrides the event file (tests, or a host that keeps local state
// elsewhere).
const PathEnv = "AGENTMUX_USAGE_EVENTS"

// Tokens is one turn's usage. Input is uncached input only: Codex reports
// input including the cached part, and the Codex reader subtracts it, so
// both runners mean the same thing. Output includes reasoning; Reasoning is
// informational and must not be added again. Requests is the number of
// model API calls, 0 when the runner does not report it (Codex).
type Tokens struct {
	Requests   int64 `json:"requests"`
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
}

// Zero reports a turn that used nothing (a hook that fired twice).
func (t Tokens) Zero() bool {
	return t.Input == 0 && t.Output == 0 && t.CacheRead == 0 && t.CacheWrite == 0
}

// Event is one finished turn.
type Event struct {
	V        int       `json:"v"`
	At       time.Time `json:"at"`
	TurnID   string    `json:"turn_id"`
	Runner   string    `json:"runner"`
	Instance string    `json:"instance"`
	Task     string    `json:"task,omitempty"`
	Run      string    `json:"run,omitempty"`
	Project  string    `json:"project,omitempty"`
	Model    string    `json:"model,omitempty"`
	Effort   string    `json:"effort,omitempty"`
	Session  string    `json:"session,omitempty"`
	Tokens
}

// EventsPath is where events go for the current user.
func EventsPath() (string, error) {
	if p := os.Getenv(PathEnv); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, DefaultPath), nil
}

// Append writes e as one line, under an exclusive lock so lines from hooks
// finishing at the same moment never interleave.
func Append(path string, e Event) error {
	if e.TurnID == "" || e.Runner == "" || e.Instance == "" {
		return fmt.Errorf("turn_id, runner and instance are required")
	}
	e.V = Version
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_, err = f.Write(append(line, '\n'))
	return err
}

// Identity is who a turn belongs to, from the instance's registry.
type Identity struct {
	Instance, Task, Run, Project, Model, Effort string
}

// IdentityFrom reads a registry's fields. forgentic stamps task_id, run_id
// and project into the instance's OTEL_RESOURCE_ATTRIBUTES at dispatch; those
// win. Otherwise project is AGENTMUX_PROJECT with any "github.com/" dropped,
// and a task-<project>-<id> instance names its task.
func IdentityFrom(instance string, fields map[string]string, model, effort string) Identity {
	id := Identity{Instance: instance, Model: model, Effort: effort}
	for _, kv := range strings.Split(fields["AGENTMUX_ENV_OTEL_RESOURCE_ATTRIBUTES"], ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "task_id":
			id.Task = v
		case "run_id":
			id.Run = v
		case "project":
			id.Project = v
		}
	}
	if id.Project == "" {
		id.Project = strings.TrimPrefix(fields["AGENTMUX_PROJECT"], "github.com/")
	}
	if id.Task == "" {
		id.Task = taskFromInstance(instance)
	}
	return id
}

// taskFromInstance turns task-mergentic-merg-415 into MERG-415: the last two
// dash-separated words, upper-cased, when the last is a number.
func taskFromInstance(instance string) string {
	rest, ok := strings.CutPrefix(instance, "task-")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 3 {
		return ""
	}
	key, num := parts[len(parts)-2], parts[len(parts)-1]
	if key == "" || num == "" || strings.Trim(num, "0123456789") != "" {
		return ""
	}
	return strings.ToUpper(key) + "-" + num
}

// Event builds an event for this identity.
func (id Identity) Event(runner, turnID, session string, t Tokens) Event {
	return Event{
		TurnID: turnID, Runner: runner, Instance: id.Instance, Task: id.Task, Run: id.Run,
		Project: id.Project, Model: id.Model, Effort: id.Effort, Session: session, Tokens: t,
	}
}
