package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/usagehook"
)

// The completed-turn usage hook (docs/completed-turn-usage.md). Claude Code
// runs it as a Stop hook from the instance's generated settings; the codex
// run launcher runs it after each `codex exec` exits. Either way it appends
// one event with the turn's tokens to the run user's turn-completions file.
// A hook never fails the turn: problems go to the errors file beside it.

// usageHookModelEnv and usageHookEffortEnv carry a codex run's -m and
// model_reasoning_effort from its argv to the hook.
const (
	usageHookBinEnv    = "AGENTMUX_USAGE_HOOK"
	usageHookModelEnv  = "AGENTMUX_USAGE_MODEL"
	usageHookEffortEnv = "AGENTMUX_USAGE_EFFORT"
)

// usageHookBinary is the agentmux binary the hooks call, or "" when this
// process is not agentmux (a test binary must never be handed to a hook,
// where it would rerun its own tests); replaceable in tests.
var usageHookBinary = func() string {
	self, err := os.Executable()
	if err != nil || filepath.Base(self) != "agentmux" {
		return ""
	}
	return self
}

// claudeUsageHookCommand is the Stop hook command for instance, or "" when
// the binary can't be found.
func claudeUsageHookCommand(instance string) string {
	bin := usageHookBinary()
	if bin == "" {
		return ""
	}
	return shellQuote(bin) + " usage-hook claude --instance " + shellQuote(instance)
}

// claudeSettingsWithUsageHook adds the Stop hook to the rendered settings
// JSON (empty means none yet). Claude Code merges hooks from every settings
// source, so the user's own Stop hooks still run.
func claudeSettingsWithUsageHook(data []byte, command string) ([]byte, error) {
	if command == "" {
		return data, nil
	}
	doc := map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return data, err
		}
	}
	doc["hooks"] = map[string]any{"Stop": []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 30}},
	}}}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return data, err
	}
	return append(out, '\n'), nil
}

// codexArgsModelEffort is the model and effort a CodexRunArgs argv asks for.
func codexArgsModelEffort(argv []string) (model, effort string) {
	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "-m", "--model":
			model = argv[i+1]
		case "-c", "--config":
			if v, ok := strings.CutPrefix(argv[i+1], "model_reasoning_effort="); ok {
				effort = strings.Trim(v, `"`)
			}
		}
	}
	return model, effort
}

// codexConfigDefaults reads the top-level model and model_reasoning_effort
// from the run user's codex config: what a run with no -m used.
func codexConfigDefaults() (model, effort string) {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ""
		}
		dir = filepath.Join(home, ".codex")
	}
	f, err := os.Open(filepath.Join(dir, "config.toml"))
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "model":
			model = v
		case "model_reasoning_effort":
			effort = v
		}
	}
	return model, effort
}

func usageHookHome() (string, error) {
	return os.UserHomeDir()
}

// usageHookFail records err beside the events file; it never returns one.
func usageHookFail(runner, instance string, err error) {
	path, perr := usagehook.EventsPath()
	if perr != nil {
		return
	}
	f, ferr := os.OpenFile(strings.TrimSuffix(path, ".jsonl")+".errors", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %s: %v\n", time.Now().UTC().Format(time.RFC3339), runner, instance, err)
}

func usageHookAppend(e usagehook.Event) error {
	path, err := usagehook.EventsPath()
	if err != nil {
		return err
	}
	return usagehook.Append(path, e)
}

// UsageHookClaude is `agentmux usage-hook claude --instance NAME`: Claude
// Code's Stop hook input on stdin names the transcript, whose new assistant
// usage becomes one event. Always exits cleanly from Claude's point of view.
func UsageHookClaude(instance string, stdin io.Reader) {
	if err := usageHookClaude(instance, stdin); err != nil {
		usageHookFail("claude-code", instance, err)
	}
}

func usageHookClaude(instance string, stdin io.Reader) error {
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return fmt.Errorf("reading hook input: %w", err)
	}
	if !filepath.IsAbs(in.TranscriptPath) {
		return fmt.Errorf("hook input has no transcript path")
	}
	home, err := usageHookHome()
	if err != nil {
		return err
	}
	turn, err := usagehook.ReadClaudeTurn(in.TranscriptPath, filepath.Join(home, usagehook.CursorDir))
	if err != nil {
		return err
	}
	if turn.Tokens.Zero() {
		return nil // fired again with nothing new
	}
	fields, _ := registry(instance)
	model := turn.Model
	if model == "" {
		model = fields[provision.ClaudeModelKey]
	}
	id := usagehook.IdentityFrom(instance, fields, model, fields[provision.ClaudeEffortKey])
	return usageHookAppend(id.Event("claude-code", "claude:"+in.SessionID+":"+turn.LastUUID, in.SessionID, turn.Tokens))
}

// UsageHookCodex is `agentmux usage-hook codex --instance NAME --run ID
// --offset N`, run by the codex launcher once codex has exited, with the
// run's stream log on stdin (the launcher's own read handle, which survives
// the log's rename to its thread name).
func UsageHookCodex(instance, run string, offset int64, log *os.File) {
	if err := usageHookCodex(instance, run, offset, log); err != nil {
		usageHookFail("codex", instance, err)
	}
}

func usageHookCodex(instance, run string, offset int64, log *os.File) error {
	info, err := log.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("codex run log is not a regular file")
	}
	r, err := usagehook.ReadCodexRun(log, info.Size(), offset)
	if err != nil {
		return err
	}
	if !r.Completed {
		return nil // failed or killed: codex reports no usage
	}
	model, effort := os.Getenv(usageHookModelEnv), os.Getenv(usageHookEffortEnv)
	if model == "" || effort == "" {
		dm, de := codexConfigDefaults()
		if model == "" {
			model = dm
		}
		if effort == "" {
			effort = de
		}
	}
	fields, _ := registry(instance)
	id := usagehook.IdentityFrom(instance, fields, model, effort)
	return usageHookAppend(id.Event("codex", "codex:"+r.Thread+":"+run, r.Thread, r.Tokens))
}

// codexUsageHookEnv is what the launcher needs to run the hook: the binary,
// and the model and effort argv asked for.
func codexUsageHookEnv(argv []string) []string {
	bin := usageHookBinary()
	if bin == "" {
		return nil
	}
	env := []string{usageHookBinEnv + "=" + bin}
	model, effort := codexArgsModelEffort(argv)
	if model != "" {
		env = append(env, usageHookModelEnv+"="+model)
	}
	if effort != "" {
		env = append(env, usageHookEffortEnv+"="+effort)
	}
	return env
}

// ParseUsageOffset parses the launcher's byte offset argument.
func ParseUsageOffset(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad offset %q", s)
	}
	return n, nil
}
