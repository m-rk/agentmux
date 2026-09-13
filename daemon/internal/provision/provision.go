// Package provision creates new agentmux instances: resolves defaults,
// runs preflight checks, writes the registry file, and installs the
// instance's systemd unit (Linux) or LaunchAgent (macOS). Native Go port
// of backends/*/install.sh and install-macos.sh.
package provision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// Options mirrors CreateInstanceRequest; see daemon/proto/agentmuxd.proto.
type Options struct {
	InstanceName    string
	Agent           string
	HostName        string
	Provider        string
	Model           string
	Workdir         string
	ResumeSessionID string
	RunUser         string
	CompactOnUpdate string // claude-code only: "", "on", or "off" — see proto doc
	BaseURL         string // zero/opencode/kilo only; see proto doc
	APIKeyEnv       string // kilo/opencode only, not zero; see proto doc
}

var identifierRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validateIdentifier(label, value string) error {
	if !identifierRE.MatchString(value) {
		return fmt.Errorf("%s must contain only letters, numbers, dots, underscores, and hyphens", label)
	}
	return nil
}

// defaultInstanceName computes the instance name used when -instance is
// left blank: "<workdir-basename>-<agent>" when a workdir was given, so a
// bare `agentmux new -y -agent=kilo -workdir=~/foo` produces "foo-kilo"
// rather than every unnamed instance colliding on one fixed name. Falls
// back to each agent's fixed default only when there's no workdir either
// to derive a name from.
func defaultInstanceName(agent, workdir string) string {
	if workdir != "" {
		return filepath.Base(workdir) + "-" + agent
	}
	switch agent {
	case "claude-code":
		return defaultClaudeCodeInstance
	case "amp":
		return defaultAmpInstance
	}
	return defaultAgentmuxInstance
}

// Create dispatches to the right agent-specific provisioner, after
// refusing to silently clobber an existing instance registered under a
// different agent. The wizard gives each agent an appropriate default, but
// this guard remains important for non-interactive callers and explicit name
// collisions.
func Create(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		name = defaultInstanceName(opts.Agent, opts.Workdir)
	}
	if err := guardAgentMismatch(name, opts.Agent); err != nil {
		return "", err
	}

	switch opts.Agent {
	case "claude-code":
		return createClaudeCode(opts)
	case "zero", "opencode", "kilo":
		return createAgentmux(opts)
	case "amp":
		return createAmp(opts)
	default:
		return "", fmt.Errorf("unsupported agent %q (want claude-code, zero, opencode, kilo, or amp)", opts.Agent)
	}
}

// guardAgentMismatch refuses to proceed if name is already in use by a
// different agent — either a registry-tracked instance (see
// existingAgentFor) or, since the registry only exists for instances this
// Go provisioner itself created, an older instance installed by
// backends/*/install.sh or install-macos.sh, which predates the registry
// entirely and so wouldn't show up in it at all (see unitFileExists).
// Re-running the provisioner for the *same* agent under the same name is
// the supported "update settings, keep everything else" workflow the bash
// installers already relied on, but a different (or unrecorded) agent
// under the same name is never a legitimate update — it's almost
// certainly a stale instance-name default that should have been changed.
func guardAgentMismatch(name, agent string) error {
	if existing, exists := existingAgentFor(name); exists {
		if existing == agent {
			return nil
		}
		return fmt.Errorf("instance %q already exists as agent %q; refusing to overwrite it as %q — pick a different instance name, or remove the existing instance first", name, existing, agent)
	}
	if unitFileExists(name) {
		return fmt.Errorf("instance %q already has a LaunchAgent/systemd unit installed (likely from an earlier install.sh/install-macos.sh run, predating this provisioner's own registry) — refusing to overwrite it as %q; pick a different instance name, or remove the existing one first", name, agent)
	}
	return nil
}

// existingAgentFor reads just the AGENTMUX_AGENT field from name's
// registry file, in the same KEY=VALUE format writeRegistry writes and
// discovery.go parses. Defaults to "claude-code" if the file exists but
// the field is absent, matching discovery.go's own default (the
// claude-code provisioners never set AGENTMUX_AGENT, since it was the only
// backend before zero/opencode).
func existingAgentFor(name string) (agent string, exists bool) {
	data, err := os.ReadFile(filepath.Join(discovery.EnvDir, name+".env"))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == "AGENTMUX_AGENT" && val != "" {
			return val, true
		}
	}
	return "claude-code", true
}

type kv struct{ key, value string }

// writeRegistry writes name.env into discovery.EnvDir in the same
// KEY=VALUE format discovery.go parses, in the given field order —
// matching the bash installers' own cat > file <<EOF field order, so
// output is diffable against them.
func writeRegistry(name string, fields []kv) (string, error) {
	for _, f := range fields {
		if f.key == "" || strings.ContainsAny(f.key, "=\r\n\x00") {
			return "", fmt.Errorf("invalid registry key %q", f.key)
		}
		if strings.ContainsAny(f.value, "\r\n\x00") {
			return "", fmt.Errorf("registry value for %s contains a line break or NUL byte", f.key)
		}
	}
	if err := os.MkdirAll(discovery.EnvDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", discovery.EnvDir, err)
	}
	path := filepath.Join(discovery.EnvDir, name+".env")
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%s=%s\n", f.key, f.value)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// machineName mirrors install.sh's machine_name(): short hostname, minus a
// trailing ".local", falling back to "linux"/"macos".
func machineName(fallback string) string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return fallback
	}
	name = strings.SplitN(name, ".", 2)[0]
	name = strings.TrimSuffix(name, ".local")
	if name == "" {
		return fallback
	}
	return name
}

// DefaultHostName returns the short host name used in remote display names
// when callers do not provide an override.
func DefaultHostName() string {
	return machineName("host")
}

// resolveHostName returns hostName unchanged if given, else the last
// explicit host name a caller on this host supplied, else "" (meaning: no
// explicit choice exists yet — callers pass that straight through to
// DisplayNameForHost, which derives and prefixes a name itself). Once any
// call resolves a non-blank value, it's remembered so the next instance
// created without a -host-name flag reuses it instead of re-deriving
// os.Hostname() from scratch.
func resolveHostName(hostName string) (string, error) {
	hostName = strings.TrimSpace(hostName)
	if hostName == "" {
		hostName = loadLastHostName()
	}
	if hostName == "" {
		return "", nil
	}
	if err := validateIdentifier("host name", hostName); err != nil {
		return "", err
	}
	saveLastHostName(hostName)
	return hostName, nil
}

// lastHostNamePath is a dotfile alongside the instance registries, so
// discovery.List's "*.env" glob skips it. It remembers the most recently
// resolved -host-name so a one-off override (e.g. dropping a cloud
// provider's "-vnic" suffix) sticks as the default for every instance
// created afterward, instead of re-deriving os.Hostname() every time.
func lastHostNamePath() string {
	return filepath.Join(discovery.EnvDir, ".last-host-name")
}

func loadLastHostName() string {
	data, err := os.ReadFile(lastHostNamePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveLastHostName(hostName string) {
	if err := os.MkdirAll(discovery.EnvDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(lastHostNamePath(), []byte(hostName+"\n"), 0o644)
}

// DisplayNameFor mirrors install.sh's default display-name heuristic:
// "<user>:<host> 🤹 <workdir-basename>", with the "<user>:" prefix omitted
// on single-real-user machines.
func DisplayNameFor(runUser, workdir string) string {
	return DisplayNameForHost(runUser, "", workdir)
}

// DisplayNameForHost applies the display-name heuristic with an optional
// caller-supplied host name. A blank host name derives one from the machine
// itself and, on a multi-user box, disambiguates it with a "<user>:"
// prefix; a non-blank one is assumed to already be a deliberate choice (an
// explicit -host-name, or one remembered from a previous instance via
// resolveHostName) and is shown exactly as given, with no added prefix.
func DisplayNameForHost(runUser, hostName, workdir string) string {
	prefix := ""
	if hostName == "" {
		if realUserCount() != 1 {
			prefix = runUser + ":"
		}
		hostName = DefaultHostName()
	}
	return fmt.Sprintf("%s%s 🤹 %s", prefix, hostName, filepath.Base(workdir))
}

// ResumableSession is one candidate Claude Code session a new instance
// could resume, mirroring ListResumableSessionsResponse's ResumableSession
// message.
type ResumableSession struct {
	SessionID    string
	LastModified time.Time
}

// ListResumable scans ~/.claude/projects/<slug>/*.jsonl for workdir — the
// same directory Claude Code itself writes session transcripts to, keyed
// by a slugified form of the working directory — and returns every
// session ID found there, newest first. Not part of any bash installer:
// there was no discovery mechanism for this before, only the
// --resume/AGENTMUX_RESUME flag accepting an opaque ID the caller already
// had to know. resumeHomeDir resolves whose home directory to look in
// (Linux: an arbitrary runUser, since the daemon runs as root; macOS:
// always the current user, runUser is ignored — see provision_linux.go/
// provision_darwin.go).
//
// A missing directory (no resumable sessions for this workdir yet) is not
// an error — it's the common case for a workdir that's never been used
// with Claude Code before.
func ListResumable(workdir, runUser string) ([]ResumableSession, error) {
	home, err := resumeHomeDir(runUser)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".claude", "projects", slugifyWorkdir(workdir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var sessions []ResumableSession
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, ResumableSession{
			SessionID:    strings.TrimSuffix(e.Name(), ".jsonl"),
			LastModified: info.ModTime(),
		})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastModified.After(sessions[j].LastModified) })
	return sessions, nil
}

// LastMessageIsCompactSummary reports whether workdir's most recently
// modified resumable session is still effectively sitting at a compact
// boundary — i.e. a previous nightly update already compacted it and
// nothing but bookkeeping has happened in it since, so sending another
// /compact would be a no-op. A workdir with no resumable sessions yet
// reports false, not an error.
func LastMessageIsCompactSummary(workdir, runUser string) (bool, error) {
	sessions, err := ListResumable(workdir, runUser)
	if err != nil {
		return false, err
	}
	if len(sessions) == 0 {
		return false, nil
	}
	home, err := resumeHomeDir(runUser)
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".claude", "projects", slugifyWorkdir(workdir), sessions[0].SessionID+".jsonl")
	lines, err := tailLines(path, minTailScanLines)
	if err != nil {
		return false, err
	}
	return atCompactBoundary(lines), nil
}

// minTailScanLines is how many raw transcript lines tailLines tries to
// collect for atCompactBoundary. A nightly compact plus resume leaves
// roughly a dozen conversation-and-bookkeeping lines behind it (see
// atCompactBoundary's doc comment), so this comfortably covers that with
// room to spare.
const minTailScanLines = 60

// transcriptEntry is the subset of a Claude Code transcript line that
// atCompactBoundary needs to tell a real conversation turn apart from
// bookkeeping and from the CLI's own injected turns.
type transcriptEntry struct {
	Type             string `json:"type"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsMeta           bool   `json:"isMeta"`
	Message          struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// text extracts a transcriptEntry's message text, whether content is a
// plain string or an array of {"type":"text","text":...} blocks.
func (e transcriptEntry) text() string {
	if len(e.Message.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.Message.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// localCommandTags are the literal XML-ish wrapper tags Claude Code's CLI
// prepends to a slash command's own echo of itself (its caveat, name, and
// output) — see isBookkeepingTurn.
var localCommandTags = []string{
	"<local-command-caveat>",
	"<command-name>",
	"<local-command-stdout>",
	"<local-command-stderr>",
}

// isBookkeepingTurn reports whether e is CLI-injected scaffolding around a
// slash command rather than real conversation content: either an
// "isMeta":true turn (covers both the /compact command's own caveat line
// and the "Continue from where you left off." resume prompt), the
// synthetic assistant reply to that resume prompt (message.model is the
// literal string "<synthetic>"), or one of the non-meta lines the CLI
// still emits to echo a local command's name/output back into the
// transcript.
func isBookkeepingTurn(e transcriptEntry) bool {
	if e.IsMeta {
		return true
	}
	if e.Type == "assistant" && e.Message.Model == "<synthetic>" {
		return true
	}
	text := e.text()
	for _, tag := range localCommandTags {
		if strings.HasPrefix(text, tag) {
			return true
		}
	}
	return false
}

// maxBookkeepingLookback bounds how many CLI-bookkeeping conversation
// turns atCompactBoundary will skip past while searching for either the
// compact-summary entry or real content. A normal nightly cycle produces
// about half a dozen of them (the /compact echo plus the resume
// exchange, see below), so this leaves a wide margin while still failing
// safe: if a future CLI version adds some new bookkeeping shape
// isBookkeepingTurn doesn't recognize, atCompactBoundary stops and
// reports "not a boundary" rather than skipping past it and scanning
// arbitrarily far back into real history looking for a compact summary
// that isn't there. The cost of that bailout is one redundant /compact,
// not a wrong answer.
const maxBookkeepingLookback = 20

// atCompactBoundary reports whether the newest real conversation turn
// among lines (oldest first, as returned by tailLines) is a compact
// summary — i.e. nothing but CLI bookkeeping has happened since the last
// nightly compact, so sending another /compact would be a no-op.
//
// It walks backward from the newest line, skipping non-message
// bookkeeping entries (attachment, last-prompt, ai-title, mode, ...) and
// up to maxBookkeepingLookback isBookkeepingTurn entries, until it finds
// either the compact-summary entry (true) or a real conversation turn
// (false).
//
// This has to look past more than just Claude Code's synthetic
// "Continue from where you left off." / "No response requested." resume
// exchange (injected every time --resume reattaches to a session sitting
// at a compact boundary — see updateClaudeCode's doc comment). The
// /compact command that produced that boundary in the first place also
// echoes itself back into the transcript as three more "user"-typed
// lines (a "<local-command-caveat>" isMeta turn, then non-meta
// "<command-name>" and "<local-command-stdout>" turns) sitting between
// the resume exchange and the actual isCompactSummary entry. An earlier
// version of this function only looked at the newest 3 conversation
// turns, which was enough for the resume exchange but not enough to also
// see past the /compact echo — so it kept finding the echo's plain
// non-meta lines first, treating them as "real" activity, and recompacted
// every single night regardless of whether anything had actually been
// said. Skipping every recognized bookkeeping shape (rather than counting
// a fixed number of turns) fixes that regardless of how many such lines
// accumulate between compacts, bounded by maxBookkeepingLookback so a
// transcript shape this doesn't recognize fails safe instead of scanning
// forever.
//
// A malformed or unparseable line (e.g. a partially-flushed write from a
// session still being written to) is skipped rather than treated as an
// error, since a false negative here just means one redundant /compact,
// not a failure.
func atCompactBoundary(lines [][]byte) bool {
	skipped := 0
	for i := len(lines) - 1; i >= 0; i-- {
		var e transcriptEntry
		if err := json.Unmarshal(lines[i], &e); err != nil {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue // bookkeeping entry, not a conversation turn
		}
		if e.IsCompactSummary {
			return true
		}
		if isBookkeepingTurn(e) {
			skipped++
			if skipped > maxBookkeepingLookback {
				return false
			}
			continue
		}
		return false
	}
	return false
}

// tailLines reads the final non-empty lines of path without loading the
// whole file into memory — Claude Code session transcripts can run into
// the tens of megabytes, and individual lines (a single large tool
// result) can themselves be over 100KB — growing the read window until it
// both has at least minLines lines and closes on a real line boundary
// (rather than stopping mid-window with a truncated fragment of a still-
// larger last line) or has covered the whole file. Returned in file order
// (oldest first).
func tailLines(path string, minLines int) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	size := info.Size()

	for window := int64(64 * 1024); ; window *= 2 {
		readSize := window
		if readSize > size {
			readSize = size
		}
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, size-readSize); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		start := 0
		if readSize < size {
			// The window's first newline ends a fragment of whatever line
			// preceded the window; drop that fragment rather than treat it
			// as a whole line. If there's no newline at all, the entire
			// window is a fragment of one still-larger line — keep growing
			// rather than return a truncated line.
			idx := bytes.IndexByte(buf, '\n')
			if idx < 0 {
				continue
			}
			start = idx + 1
		}
		var lines [][]byte
		for _, l := range bytes.Split(bytes.TrimRight(buf[start:], "\n"), []byte("\n")) {
			if len(l) > 0 {
				lines = append(lines, l)
			}
		}
		if len(lines) >= minLines || readSize == size {
			return lines, nil
		}
	}
}

// slugifyWorkdir mirrors Claude Code's own (undocumented, empirically
// confirmed) project-directory naming: every "/" and "." becomes "-".
func slugifyWorkdir(workdir string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(workdir)
}
