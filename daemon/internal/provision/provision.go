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
	Provider        string
	Model           string
	Workdir         string
	ResumeSessionID string
	RunUser         string
	CompactOnUpdate string // claude-code only: "", "on", or "off" — see proto doc
}

var identifierRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validateIdentifier(label, value string) error {
	if !identifierRE.MatchString(value) {
		return fmt.Errorf("%s must contain only letters, numbers, dots, underscores, and hyphens", label)
	}
	return nil
}

// Create dispatches to the right agent-specific provisioner, after
// refusing to silently clobber an existing instance registered under a
// different agent. The wizard form's instance-name field doesn't update
// when the agent selection changes — it always starts at "claude-code"
// regardless of which agent is picked — so choosing zero/opencode and
// submitting without noticing/changing that default would otherwise
// overwrite a same-named claude-code instance's registry file and
// systemd unit/LaunchAgent outright.
func Create(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		if opts.Agent == "claude-code" {
			name = defaultClaudeCodeInstance
		} else {
			name = defaultAgentmuxInstance
		}
	}
	if err := guardAgentMismatch(name, opts.Agent); err != nil {
		return "", err
	}

	switch opts.Agent {
	case "claude-code":
		return createClaudeCode(opts)
	case "zero", "opencode":
		return createAgentmux(opts)
	default:
		return "", fmt.Errorf("unsupported agent %q (want claude-code, zero, or opencode)", opts.Agent)
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

// displayNameFor mirrors install.sh's default display-name heuristic:
// "<user>:<host> 🤹 <workdir-basename>", with the "<user>:" prefix omitted
// on single-real-user machines.
func displayNameFor(runUser, workdir string) string {
	prefix := ""
	if realUserCount() != 1 {
		prefix = runUser + ":"
	}
	return fmt.Sprintf("%s%s 🤹 %s", prefix, machineName("host"), filepath.Base(workdir))
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
// modified resumable session already ends with a compact-boundary message
// — i.e. a previous nightly update already compacted it and nothing has
// happened in it since, so sending another /compact would be a no-op
// (Claude Code itself refuses with "Not enough messages to compact.",
// which otherwise wastes a nightly maintenance cycle waiting on a prompt
// that was never going anywhere). A workdir with no resumable sessions
// yet reports false, not an error.
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
	line, err := lastLine(path)
	if err != nil {
		return false, err
	}
	return isCompactSummaryLine(line), nil
}

// isCompactSummaryLine reports whether line is a Claude Code transcript
// entry with "isCompactSummary":true. A malformed or unparseable line
// (e.g. a partially-flushed write from a session still being written to)
// is treated as "not a compact summary" rather than an error, since a
// false negative here just means one redundant /compact, not a failure.
func isCompactSummaryLine(line []byte) bool {
	var entry struct {
		IsCompactSummary bool `json:"isCompactSummary"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return false
	}
	return entry.IsCompactSummary
}

// lastLine reads the final non-empty line of path without loading the
// whole file into memory — Claude Code session transcripts can run into
// the tens of megabytes, and individual lines (a single large tool
// result) can themselves be over 100KB, so the read window doubles until
// it contains a newline rather than assuming a fixed chunk size suffices.
func lastLine(path string) ([]byte, error) {
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
		trimmed := bytes.TrimRight(buf, "\n")
		if idx := bytes.LastIndexByte(trimmed, '\n'); idx >= 0 {
			return trimmed[idx+1:], nil
		}
		if readSize == size {
			return trimmed, nil // whole file is a single line (or empty)
		}
	}
}

// slugifyWorkdir mirrors Claude Code's own (undocumented, empirically
// confirmed) project-directory naming: every "/" and "." becomes "-".
func slugifyWorkdir(workdir string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(workdir)
}
