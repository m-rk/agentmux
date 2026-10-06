package ops

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/ampconfig"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// RunRequest starts an amp thread on an instance (Thread "") or continues
// one (Thread set) by running the prompt through the amp CLI in the
// instance's workdir. Title names a new thread ("<task id> <task name>"
// from the dispatcher); a continue ignores it. Labels ride every run as
// `amp -l` (repeatable): they land on the created thread and re-apply to
// a continued one, so `amp threads list --label <x>` finds worker
// threads. The CLI runs detached: Run returns as soon as the stream init
// record arrives, while the agent keeps working.
type RunRequest struct {
	Address string // <instance>@<host>[#<thread>]
	Text    string // the prompt; read from -file by the CLI
	Title   string // thread title for a new thread; "" leaves amp's own
	// Labels ride `amp -l` on every run, new or continued; "" or junk
	// entries are dropped by session.CleanAmpLabels, never a refusal.
	Labels []string
	// Mode is an explicit per-run -m override (sessions run -mode):
	// when set it wins over the instance override and the host file.
	// AMUX-36's Require applies only when this is empty.
	Mode string
	// Template names an existing amp instance on this host whose
	// registry, workdir, mode and thread path a dry run validates
	// against instead of the target's: a dry-run create makes nothing,
	// so a smoke-test target may not exist yet (see AMUX-27). Dry-run
	// only; a real run with Template set is refused.
	Template string
	// DryRun validates everything a real run would — address, text,
	// instance, workdir, host config, mode shape, thread id and title —
	// but starts no amp thread. The result carries DryRun and Plan.
	DryRun bool
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
	// DryRun is set when nothing was started; Plan lists what would
	// happen, dry-run only.
	DryRun bool     `json:"dry_run,omitempty"`
	Plan   []string `json:"plan,omitempty"`
}

// AmpModeInfo is the effective amp mode for `sessions status -json`.
type AmpModeInfo struct {
	// Mode is the `-m` value runs use; empty when none is configured.
	Mode string `json:"mode,omitempty"`
	// Source is "instance", "host", or "" (no mode configured anywhere).
	Source string `json:"source,omitempty"`
	// Label is the host's display name for the mode; empty when unconfigured.
	Label string `json:"label,omitempty"`
	// Short is the host's short button text; empty when unconfigured.
	Short string `json:"short,omitempty"`
	// Emoji are the host's display emoji names (first provider, last
	// model); absent when unconfigured.
	Emoji []string `json:"emoji,omitempty"`
}

// ampThreadURLPrefix is the public address of one amp thread.
const ampThreadURLPrefix = "https://ampcode.com/threads/"

// runTimeout bounds a run: mode check plus spawn plus init record. A bogus
// -m fails in seconds (confirmed live ~8s); the bound is for a hung
// launch, not the mode-rejection path.
const runTimeout = 3 * time.Minute

// AmpModeOf resolves the effective amp mode for an instance: the
// instance's AGENTMUX_AMP_MODE registry value, else the host's
// ~/.config/agentmux/amp.yaml. Exported so Status reports it. The display
// keys (label/short/emoji) always come from the host file — an instance
// override changes only the -m value — and stay empty when unconfigured.
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
	label, short, emoji := ampconfig.DisplayOf(host)
	return AmpModeInfo{Mode: mode, Source: source, Label: label, Short: short, Emoji: emoji}
}

// runTarget resolves which instance's registry entry a run validates
// against. A plain run (or a templated run whose template is the target
// itself) reads the target's own existing entry — missing is refused as
// not_found. A dry run with Template naming another instance validates
// the target's run readiness against the template: a dry-run create makes
// nothing, so the smoke-test target may not exist yet, and reporting
// not_found for the name the create just named would fail every smoke
// test (AMUX-27). Template is dry-run only: a real run with one set is
// refused as invalid, and a templated dry run with a thread suffix is
// too — a continue needs the thread's own instance to exist.
func runTarget(addr address.Address, req RunRequest) (name string, allowMissing bool, err error) {
	if req.Template == "" {
		return addr.Instance, false, nil
	}
	if !req.DryRun {
		return "", false, Refuse(safesend.ReasonInvalid, "template %q is dry-run only", req.Template)
	}
	if t := strings.TrimSpace(addr.Thread); t != "" {
		return "", false, Refuse(safesend.ReasonInvalid, "template %q cannot continue thread %q: the thread's own instance must exist", req.Template, t)
	}
	tmpl, terr := parseLocal(req.Template + "@" + addr.Host)
	if terr != nil {
		return "", false, terr
	}
	if tmpl.Thread != "" {
		return "", false, Refuse(safesend.ReasonInvalid, "template %q must name an instance, not a thread", req.Template)
	}
	if tmpl.Instance == addr.Instance {
		return addr.Instance, false, nil
	}
	return tmpl.Instance, true, nil
}

// runInstanceFields reads one instance's registry entry for a run: the
// fields to validate plus the source behind label, the reported
// address (the target's, not the template's). Missing entries are
// refused as not_found; non-amp instances, and amp ones with no runner
// id, as unsupported.
func runInstanceFields(name, label string) (map[string]string, transcript.Source, error) {
	src, _, err := Source(address.Address{Instance: name})
	if err != nil {
		return nil, transcript.Source{}, err
	}
	if src.Agent != "amp" {
		return nil, transcript.Source{}, Refuse(safesend.ReasonUnsupported, "sessions run is only for amp instances; %s is %q", label, src.Agent)
	}
	if src.AmpRunnerID == "" {
		return nil, transcript.Source{}, Refuse(safesend.ReasonUnsupported, "%s has no amp runner id", label)
	}
	fields, err := session.ReadRegistry(name)
	if err != nil {
		return nil, transcript.Source{}, Refuse(safesend.ReasonNotFound, "no instance %q on this host", name)
	}
	return fields, src, nil
}

// runDryTarget checks the smoke-test target side of a templated dry run:
// the target name must be a valid instance name a real create could make
// (a clash with an existing instance is the create's own refusal). The
// template's workdir, mode and thread path are validated by the shared
// path below, exactly as for a plain run — the target inherits them from
// the create — so there is no target-side path to probe beyond the name.
func runDryTarget(addr address.Address) error {
	if err := provision.ValidateInstanceName(addr.Instance); err != nil || strings.HasPrefix(addr.Instance, ".") {
		return Refuse(safesend.ReasonInvalid, "instance %q: want letters, numbers, dots, underscores and hyphens, not starting with a dot", addr.Instance)
	}
	return nil
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
	if reqThread := strings.TrimSpace(addr.Thread); reqThread != "" {
		if !transcript.ValidAmpThreadID(reqThread) {
			return RunResult{}, Refuse(safesend.ReasonInvalid, "%q is not an amp thread id", reqThread)
		}
	}
	target, allowMissing, err := runTarget(addr, req)
	if err != nil {
		return RunResult{}, err
	}
	var fields map[string]string
	var src transcript.Source
	if allowMissing {
		// The smoke-test target may not exist yet: validate its run
		// readiness against the template instead, and never report the
		// target as not_found (see runTarget). The template lookup
		// refuses first when the template itself is missing, so a bad
		// template can never pass as a missing target.
		var terr error
		fields, src, terr = runInstanceFields(target, addr.Instance)
		if terr != nil {
			return RunResult{}, terr
		}
		if err := runDryTarget(addr); err != nil {
			return RunResult{}, err
		}
	} else {
		var rerr error
		fields, src, rerr = runInstanceFields(target, addr.Instance)
		if rerr != nil {
			return RunResult{}, rerr
		}
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
	// No amp thread starts without -m (AMUX-36): a missing mode
	// everywhere is a refusal, never a silent run on amp's default
	// model. The -mode flag overrides both, per run.
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		var rerr error
		mode, _, rerr = ampconfig.Require(host, fields[ampconfig.EnvOverride])
		if rerr != nil {
			return RunResult{}, Refuse(safesend.ReasonFailed, "%v", rerr)
		}
	}

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
	labels := session.CleanAmpLabels(req.Labels)
	if req.DryRun {
		// A dry run validated everything a real run would — address,
		// text, amp instance and runner, workdir, host config, mode
		// shape, thread id and title — and stops before spawning
		// anything: no unarchive, no stop, no amp thread, no log. With
		// a template the plan says what the validation stood in for:
		// the template exists and is amp, its workdir/mode/thread
		// path check out, and the target name is one a real create
		// could make — the checks a real run of the smoke name would
		// need, without the smoke instance existing yet.
		step := "start amp thread on " + addr.Session().String()
		if thread != "" {
			step = "continue amp thread " + addr.String()
		}
		if mode != "" {
			step += " with mode " + mode
		}
		if len(labels) > 0 {
			step += " with labels " + strings.Join(labels, ",")
		}
		if req.Template != "" && target != addr.Instance {
			step += " (validated against template " + target + ")"
		}
		return RunResult{
			OK: true, Address: req.Address, Agent: "amp",
			DryRun: true, Plan: []string{step},
		}, nil
	}
	if thread == "" && title != "" {
		// The kickoff notification shows the first message, not the
		// sidebar title, so the prompt opens with a one-line
		// "<title>" header naming the task before anything else. A
		// title is at most 256 bytes against a 64 KiB prompt, and
		// CleanAmpTitle already rejected line breaks, so the header is
		// always exactly one line.
		text = title + "\n\n" + text
	}
	logPath := session.AmpRunLogPath(src.Home, addr.Instance, thread)
	if thread != "" {
		// Continuing restores the archived thread first (amp archives a
		// thread when an `-x` run ends, and continuing an archived one
		// refuses), and stops the stuck process behind a pending
		// `ask_user_choice` question: in `-x` mode nothing can answer
		// that dialog, so the run just waits, and the old process would
		// otherwise keep holding the thread. Both are best-effort — the
		// continue proceeds either way.
		if st := session.AmpRunStateOf(logPath); st.State == "waiting" {
			session.StopAmpRun(ctx, thread, workdir)
		}
		session.UnarchiveAmpThread(ctx, src.AmpEnvFile, thread)
	}
	id, err := session.StartAmpRun(ctx, addr.Instance, src.AmpEnvFile, session.AmpRunArgs(text, mode, thread, title, labels), workdir, logPath)
	if err != nil {
		// The real run is also the mode check now: a name amp rejects
		// dies here before printing its init record, quoting amp's own
		// error (confirmed live: a bogus -m fails fast with no thread
		// created server-side — the log tail carries amp's "Unexpected
		// error inside Amp CLI"). Invalid, not failed — retrying the same
		// mode cannot succeed.
		if mode != "" && strings.Contains(err.Error(), "Unexpected error inside Amp CLI") {
			return RunResult{}, Refuse(safesend.ReasonInvalid, "amp rejected mode %q: %v", mode, err)
		}
		return RunResult{}, Refuse(safesend.ReasonFailed, "amp: %v", err)
	}
	// The thread keeps the task's title against amp's auto-title, which
	// replaces `--title` with its own summary while the agent works:
	// rename once now (best-effort, never a refusal), and once more in
	// the background after the agent's first assistant record lands —
	// the point where the overwrite happens. A continue re-applies a
	// given title too, since the worker's own turns overwrite it in
	// between. The finished thread stays unarchived (see AmpRunArgs),
	// so it can still be found and renamed.
	threadLog := logPath
	if thread == "" {
		// The log was kept under a pending name until the thread id was
		// known; move it under the thread's own name so status and read
		// find it — and so the background re-title watches the same
		// file the agent appends to.
		threadLog = session.AmpRunLogPath(src.Home, addr.Instance, id)
		_ = os.Rename(logPath, threadLog)
	}
	if title != "" {
		session.RenameAmpThread(ctx, src.AmpEnvFile, id, title)
		session.RetitleSpawn(context.WithoutCancel(ctx), src.AmpEnvFile, id, title, threadLog)
	}
	full := address.Address{Instance: addr.Instance, Host: addr.Host, Thread: id}
	res := RunResult{
		OK: true, Address: full.String(), Agent: "amp",
		Thread: id, ThreadID: id, ThreadURL: ampThreadURLPrefix + id,
		State: "running",
	}
	return res, nil
}
