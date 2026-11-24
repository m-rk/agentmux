package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/usagehook"
)

// maxOutput caps what is kept of a runner's stdout or stderr.
const maxOutput = 1 << 20

var binaries = map[string]string{
	catalogue.RunnerClaudeCode: "claude",
	catalogue.RunnerCodex:      "codex",
	catalogue.RunnerAmp:        "amp",
	catalogue.RunnerOpencode:   "opencode",
	catalogue.RunnerKilo:       "kilo",
}

// Args is the command line of one try. Every runner is asked once,
// headless, with no tools and nothing saved where agentmux looks.
func Args(t Target) []string {
	switch t.Runner {
	case catalogue.RunnerClaudeCode:
		return []string{"-p", Prompt, "--model", t.Model, "--output-format", "json",
			"--no-session-persistence", "--tools", "", "--strict-mcp-config",
			"--disable-slash-commands", "--system-prompt", Prompt}
	case catalogue.RunnerCodex:
		a := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "-m", t.Model}
		if t.Effort != "" {
			a = append(a, "-c", "model_reasoning_effort="+t.Effort)
		}
		return append(a, Prompt)
	case catalogue.RunnerAmp:
		// --stream-json must come before -x, which takes the next word
		// as its message (see session/amprun.go).
		return []string{"--stream-json", "-m", t.Model, "-x", Prompt}
	case catalogue.RunnerOpencode, catalogue.RunnerKilo:
		return []string{"run", "--format", "json", "-m", t.Model, Prompt}
	}
	return nil
}

// SetupFunc writes an opencode or kilo instance's provider config into dir
// and returns the provider env (session.ProbeSetup).
type SetupFunc func(instance, dir string) ([]string, error)

// Exec is the real TryFunc: one runner process in an empty temporary
// directory, as the current user. opencode and kilo get the provider
// config of the target's instance written there, and their own data and
// state directories inside it, so the probe's session lands nowhere an
// instance looks.
func Exec(setup SetupFunc) TryFunc {
	return func(ctx context.Context, t Target) Attempt { return execOnce(ctx, setup, t) }
}

func execOnce(ctx context.Context, setup SetupFunc, t Target) Attempt {
	bin := binaries[t.Runner]
	if _, err := runas.CurrentUserLookPath(bin); bin == "" || err != nil {
		return Attempt{Err: &Failure{Kind: catalogue.ErrKindOther, Words: t.Runner + " is not installed"}}
	}
	dir, err := os.MkdirTemp("", "agentmux-probe-")
	if err != nil {
		return Attempt{Err: &Failure{Kind: catalogue.ErrKindOther, Words: "could not make a working directory"}}
	}
	defer os.RemoveAll(dir)
	cctx, cancel := context.WithTimeout(ctx, TryTimeout)
	defer cancel()
	cmd := runas.CurrentUserCommandContext(cctx, bin, Args(t)...)
	cmd.Dir = dir
	cmd.Env = probeEnv(cmd.Env)
	if t.Runner == catalogue.RunnerOpencode || t.Runner == catalogue.RunnerKilo {
		if t.Instance == "" || setup == nil {
			return Attempt{Err: &Failure{Kind: catalogue.ErrKindOther, Words: "no instance in the catalogue carries this provider's config"}}
		}
		extra, err := setup(t.Instance, dir)
		if err != nil {
			return Attempt{Err: &Failure{Kind: catalogue.ErrKindOther, Words: "could not read the provider config of its instance"}}
		}
		cmd.Env = append(cmd.Env, extra...)
		cmd.Env = append(cmd.Env, "XDG_DATA_HOME="+dir+"/.data", "XDG_STATE_HOME="+dir+"/.state")
	}
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	runErr := cmd.Run()
	a := Parse(t.Runner, stdout.Bytes())
	a.Latency = time.Since(start)
	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	a.Err = Judge(a.Err, runErr, timedOut, stderr.String())
	return a
}

// probeEnv drops the variables that make a CLI think it is nested inside
// another agent session.
func probeEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// Judge turns how a try ended into a Failure, or nil for a pass. parsed
// is what Parse made of stdout: nil when the reply was OK. The output is
// only read for its class; none of it is kept.
func Judge(parsed *Failure, runErr error, timedOut bool, stderr string) *Failure {
	if timedOut {
		return &Failure{Kind: catalogue.ErrKindTimeout, Words: fmt.Sprintf("no reply within %s", TryTimeout)}
	}
	if parsed == nil {
		return nil // the reply was OK, whatever the exit status
	}
	text := parsed.Words + "\n" + stderr
	if f := Classify(text); f != nil {
		return f
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return &Failure{Kind: catalogue.ErrKindOther, Words: fmt.Sprintf("runner exited with status %d", ee.ExitCode())}
	}
	if runErr != nil {
		return &Failure{Kind: catalogue.ErrKindOther, Words: "runner could not start"}
	}
	return &Failure{Kind: catalogue.ErrKindOther, Words: "reply was not OK"}
}

var classes = []struct {
	kind, words string
	needles     []string
}{
	{catalogue.ErrKindQuota, "usage limit or quota reached", []string{"429", "rate limit", "rate_limit", "usage limit", "quota", "insufficient", "credit", "billing", "limit reached", "too many requests", "overloaded"}},
	{catalogue.ErrKindAuth, "not signed in, or the login was refused", []string{"401", "403", "unauthorized", "unauthenticated", "not logged in", "log in", "login", "authentication", "api key", "invalid_api_key", "credentials", "token expired", "expired token", "forbidden", "permission denied"}},
	{catalogue.ErrKindNetwork, "provider unreachable over the network", []string{"econnrefused", "connection refused", "econnreset", "connection reset", "no such host", "enotfound", "dial tcp", "network", "tls handshake", "socket hang up", "could not resolve", "503", "502", "bad gateway", "service unavailable"}},
	{catalogue.ErrKindTimeout, "the provider timed out", []string{"timed out", "timeout", "deadline exceeded", "504"}},
}

// Classify finds the class of an error text, or nil if none fits.
func Classify(text string) *Failure {
	low := strings.ToLower(text)
	for _, c := range classes {
		for _, n := range c.needles {
			if strings.Contains(low, n) {
				return &Failure{Kind: c.kind, Words: c.words}
			}
		}
	}
	return nil
}

// IsOK reports whether a reply is the word OK.
func IsOK(reply string) bool {
	r := strings.ToLower(strings.Trim(strings.TrimSpace(reply), ".!\"'`*"))
	return r == "ok" || strings.HasPrefix(r, "ok ") || strings.HasPrefix(r, "ok.") || strings.HasPrefix(r, "ok\n")
}

// Parse reads a runner's stdout: the tokens, its session id, and a
// Failure carrying the runner's own error text (for Classify only) when
// the reply was not OK.
func Parse(runner string, out []byte) Attempt {
	var a Attempt
	var reply, errText string
	switch runner {
	case catalogue.RunnerClaudeCode:
		var r struct {
			IsError   bool         `json:"is_error"`
			Result    string       `json:"result"`
			SessionID string       `json:"session_id"`
			Usage     anthropicUse `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(out), &r) == nil {
			a.Session, a.Tokens = r.SessionID, r.Usage.tokens()
			a.Tokens.Requests = 1
			if r.IsError {
				errText = r.Result
			} else {
				reply = r.Result
			}
		}
	case catalogue.RunnerCodex:
		eachLine(out, func(line []byte) {
			var r struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
				Message  string `json:"message"`
				Item     struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				Usage *struct {
					Input     int64 `json:"input_tokens"`
					Cached    int64 `json:"cached_input_tokens"`
					Output    int64 `json:"output_tokens"`
					Reasoning int64 `json:"reasoning_output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(line, &r) != nil {
				return
			}
			switch r.Type {
			case "thread.started":
				a.Session = r.ThreadID
			case "item.completed":
				if r.Item.Type == "agent_message" {
					reply = r.Item.Text
				}
			case "turn.completed":
				if r.Usage != nil {
					a.Tokens = usagehook.Tokens{Input: r.Usage.Input - r.Usage.Cached, CacheRead: r.Usage.Cached, Output: r.Usage.Output, Reasoning: r.Usage.Reasoning}
				}
			case "error":
				errText += r.Message + "\n"
			case "turn.failed":
				errText += r.Error.Message + "\n"
			}
		})
	case catalogue.RunnerAmp:
		eachLine(out, func(line []byte) {
			var r struct {
				Type      string `json:"type"`
				Subtype   string `json:"subtype"`
				SessionID string `json:"session_id"`
				IsError   bool   `json:"is_error"`
				Result    string `json:"result"`
				Error     string `json:"error"`
				Message   *struct {
					Usage   *anthropicUse `json:"usage"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &r) != nil {
				return
			}
			switch r.Type {
			case "system":
				if r.SessionID != "" {
					a.Session = r.SessionID
				}
			case "assistant":
				if r.Message != nil && r.Message.Usage != nil {
					u := r.Message.Usage.tokens()
					a.Tokens.Input += u.Input
					a.Tokens.Output += u.Output
					a.Tokens.CacheRead += u.CacheRead
					a.Tokens.CacheWrite += u.CacheWrite
					a.Tokens.Requests++
				}
			case "result":
				if r.IsError {
					errText = r.Error + "\n" + r.Result
				} else {
					reply = r.Result
				}
			}
		})
	case catalogue.RunnerOpencode, catalogue.RunnerKilo:
		eachLine(out, func(line []byte) {
			var r struct {
				Type      string          `json:"type"`
				SessionID string          `json:"sessionID"`
				Error     json.RawMessage `json:"error"`
				Part      struct {
					Text   string `json:"text"`
					Tokens *struct {
						Input     int64 `json:"input"`
						Output    int64 `json:"output"`
						Reasoning int64 `json:"reasoning"`
						Cache     struct {
							Read  int64 `json:"read"`
							Write int64 `json:"write"`
						} `json:"cache"`
					} `json:"tokens"`
				} `json:"part"`
			}
			if json.Unmarshal(line, &r) != nil {
				return
			}
			if r.SessionID != "" {
				a.Session = r.SessionID
			}
			switch r.Type {
			case "text":
				reply += r.Part.Text
			case "step_finish":
				if tk := r.Part.Tokens; tk != nil {
					a.Tokens.Input += tk.Input
					a.Tokens.Output += tk.Output
					a.Tokens.Reasoning += tk.Reasoning
					a.Tokens.CacheRead += tk.Cache.Read
					a.Tokens.CacheWrite += tk.Cache.Write
					a.Tokens.Requests++
				}
			case "error":
				errText += string(r.Error) + "\n"
			}
		})
	}
	if !IsOK(reply) {
		a.Err = &Failure{Kind: catalogue.ErrKindOther, Words: errText}
	}
	return a
}

type anthropicUse struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicUse) tokens() usagehook.Tokens {
	return usagehook.Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

func eachLine(out []byte, f func([]byte)) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), maxOutput)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 && line[0] == '{' {
			f(line)
		}
	}
}

// UsageEvent is the usage-file line for one try, or false when the try
// reported no tokens.
func UsageEvent(t Target, host string, try int, a Attempt, at time.Time) (usagehook.Event, bool) {
	if a.Tokens.Zero() {
		return usagehook.Event{}, false
	}
	id := usagehook.Identity{Instance: "models-probe", Model: t.Model, Effort: t.Effort}
	ev := id.Event(t.Runner, fmt.Sprintf("probe:%s:%s:%d:%d", t.Provider, host, at.UTC().Unix(), try), a.Session, a.Tokens)
	ev.At = at.UTC()
	return ev, true
}
