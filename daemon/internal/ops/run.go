package ops

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/ampconfig"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// RunRequest starts an amp thread on an instance (Thread "") or continues
// one (Thread set) by running the prompt through the amp CLI in the
// instance's workdir. Title names a new thread ("<task id> <task name>"
// from the dispatcher); a continue ignores it. The CLI runs detached: Run
// returns as soon as the stream init record arrives, while the agent keeps
// working.
type RunRequest struct {
	Address string // <instance>@<host>[#<thread>]
	Text    string // the prompt; read from -file by the CLI
	Title   string // thread title for a new thread; "" leaves amp's own
}

// RunResult is the thread, plus its state when already known.
type RunResult struct {
	OK       bool   `json:"ok"`
	Address  string `json:"address"`
	Agent    string `json:"agent,omitempty"`
	Thread   string `json:"thread,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	// ThreadURL is the public amp address of the thread.
	ThreadURL string `json:"thread_url,omitempty"`
	// State is always "running": the agent was just launched (or
	// relaunched for a continue) and the caller polls `sessions status`
	// for what happens next.
	State string `json:"state,omitempty"`
}

// AmpModeInfo is the effective amp mode for `sessions status -json`.
type AmpModeInfo struct {
	// Mode is the `-m` value runs use; empty when none is configured.
	Mode string `json:"mode,omitempty"`
	// Source is "instance", "host", or "" (no mode configured anywhere).
	Source string `json:"source,omitempty"`
}

// ampThreadURLPrefix is the public address of one amp thread.
const ampThreadURLPrefix = "https://ampcode.com/threads/"

// runTimeout bounds a run: mode probe plus spawn plus init record. A bogus
// -m fails in seconds (confirmed live ~8s); the bound is for a hung
// launch, not the mode-rejection path.
const runTimeout = 3 * time.Minute

// AmpModeOf resolves the effective amp mode for an instance: the
// instance's AGENTMUX_AMP_MODE registry value, else the host's
// ~/.config/agentmux/amp.yaml. Exported so Status reports it.
func AmpModeOf(instance string) AmpModeInfo {
	var instanceMode string
	if fields, err := session.ReadRegistry(instance); err == nil {
		instanceMode = fields[ampconfig.EnvOverride]
	}
	host, err := ampconfig.Load(ampconfig.DefaultPath())
	if err != nil {
		host = ampconfig.Config{}
	}
	mode, source := ampconfig.Resolve(host, instanceMode)
	return AmpModeInfo{Mode: mode, Source: source}
}

// Run starts (or continues) an amp thread on this host. Only amp
// instances have a run path: every other agent is refused as unsupported.
func (e Env) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	addr, err := parseLocal(req.Address)
	if err != nil {
		return RunResult{}, err
	}
	text, err := CleanText(req.Text)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	src, _, err := Source(addr)
	if err != nil {
		return RunResult{}, err
	}
	if src.Agent != "amp" {
		return RunResult{}, Refuse(safesend.ReasonUnsupported, "sessions run is only for amp instances; %s is %q", addr.Instance, src.Agent)
	}
	if src.AmpRunnerID == "" {
		return RunResult{}, Refuse(safesend.ReasonUnsupported, "%s has no amp runner id", addr.Instance)
	}
	if reqThread := strings.TrimSpace(addr.Thread); reqThread != "" {
		if !transcript.ValidAmpThreadID(reqThread) {
			return RunResult{}, Refuse(safesend.ReasonInvalid, "%q is not an amp thread id", reqThread)
		}
	}
	fields, err := session.ReadRegistry(addr.Instance)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonNotFound, "no instance %q on this host", addr.Instance)
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	if workdir == "" {
		return RunResult{}, Refuse(safesend.ReasonUnsupported, "%s has no workdir", addr.Instance)
	}
	if info, err := os.Stat(workdir); err != nil || !info.IsDir() {
		return RunResult{}, Refuse(safesend.ReasonFailed, "workdir %s is not a directory", workdir)
	}
	host, herr := ampconfig.Load(ampconfig.DefaultPath())
	if herr != nil {
		// A broken host file is caller-visible only when it would matter:
		// with an instance override the host file is never consulted.
		if strings.TrimSpace(fields[ampconfig.EnvOverride]) == "" {
			return RunResult{}, Refuse(safesend.ReasonFailed, "reading amp host config: %v", herr)
		}
		host = ampconfig.Config{}
	}
	mode, _ := ampconfig.Resolve(host, fields[ampconfig.EnvOverride])

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	if err := session.CheckAmpMode(ctx, src.AmpEnvFile, mode); err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	thread := strings.TrimSpace(addr.Thread)
	title, err := session.CleanAmpTitle(req.Title)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}
	logPath := session.AmpRunLogPath(src.Home, addr.Instance, thread)
	id, err := session.StartAmpRun(ctx, src.AmpEnvFile, session.AmpRunArgs(text, mode, thread, title), workdir, logPath)
	if err != nil {
		return RunResult{}, Refuse(safesend.ReasonFailed, "amp: %v", err)
	}
	// A new thread keeps the task's title even if amp retitles it while
	// working: best-effort, never a refusal. A continue ignores the
	// title; the finished thread stays unarchived (see AmpRunArgs), so it
	// can still be found and renamed.
	if thread == "" {
		session.RenameAmpThread(ctx, src.AmpEnvFile, id, title)
	}
	full := address.Address{Instance: addr.Instance, Host: addr.Host, Thread: id}
	res := RunResult{
		OK: true, Address: full.String(), Agent: "amp",
		Thread: id, ThreadID: id, ThreadURL: ampThreadURLPrefix + id,
		State: "running",
	}
	if thread == "" {
		// The log was kept under a pending name until the thread id was
		// known; move it under the thread's own name so status and read
		// find it.
		_ = os.Rename(logPath, session.AmpRunLogPath(src.Home, addr.Instance, id))
	}
	return res, nil
}
