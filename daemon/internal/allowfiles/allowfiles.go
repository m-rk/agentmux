// Package allowfiles implements `agentmux new -allow-file`: letting an
// agent read and edit a few individual files outside its workdir (a task
// note in a notes vault, say) without opening up the directories they live
// in. This file holds the agent-independent part — validation and the
// registry encoding; claude.go renders Claude Code's settings. See
// docs/allow-file.md.
package allowfiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxFiles bounds how many files one instance may be granted.
const MaxFiles = 20

// RegistryKey is the registry field holding the grant list, as a
// single-line JSON array of absolute paths.
const RegistryKey = "AGENTMUX_ALLOW_FILES"

// Supported reports whether agent applies allow-files at launch. This is
// the seam for other agents: add a case here once its generator exists;
// provisioning warns for every agent that returns false.
func Supported(agent string) bool {
	switch agent {
	case "claude-code":
		return true
	}
	return false
}

// Validate checks paths for a new or updated instance and returns the
// paths to grant: each one resolved through symlinks to the real file, so
// the grant is exactly what the agent can reach and a symlink can't be
// retargeted later to widen it. Every path must be absolute and already
// clean, name an existing regular file (not a directory), lie outside
// workdir (pointless inside it), and avoid characters that agent
// permission rules treat as patterns and can't be escaped reliably.
// Duplicates collapse. Strict at `new` time; launch uses Existing instead
// so a renamed file doesn't wedge the instance.
func Validate(paths []string, workdir string) ([]string, error) {
	if len(paths) > MaxFiles {
		return nil, fmt.Errorf("too many -allow-file paths: %d (max %d)", len(paths), MaxFiles)
	}
	var wd []string
	if workdir != "" {
		wd = append(wd, filepath.Clean(workdir))
		if real, err := filepath.EvalSymlinks(workdir); err == nil {
			wd = append(wd, real)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		real, err := resolve(p)
		if err != nil {
			return nil, err
		}
		for _, d := range wd {
			if real == d || strings.HasPrefix(real, d+string(filepath.Separator)) {
				return nil, fmt.Errorf("-allow-file %q is inside the workdir %s; it is already accessible there", p, workdir)
			}
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, real)
		}
	}
	return out, nil
}

// resolve validates one path and returns its symlink-resolved real path.
func resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("-allow-file: empty path")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("-allow-file %q: must be an absolute path", p)
	}
	if filepath.Clean(p) != p {
		return "", fmt.Errorf("-allow-file %q: must be a clean path (no '..', '.', '//' or trailing '/'; want %q)", p, filepath.Clean(p))
	}
	if err := checkName(p); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("-allow-file %q: %w", p, err)
	}
	if err := checkName(real); err != nil {
		return "", fmt.Errorf("-allow-file %q resolves to %q: %w", p, real, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("-allow-file %q: %w", p, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("-allow-file %q: not a regular file (directories and special files can't be granted)", p)
	}
	return real, nil
}

// checkName rejects characters that would make a permission rule match
// more than the one file: '?' and braces are live pattern syntax that
// Claude Code gives no working escape for (verified: "\?" and "[?]" don't
// match the literal), backslash is the escape character itself, and
// control characters can't live in a single-line registry value.
func checkName(p string) error {
	for _, r := range p {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`?{}\`, r) {
			return fmt.Errorf("-allow-file %q: path contains %q, which can't be granted safely", p, r)
		}
	}
	return nil
}

// Existing is the launch-time counterpart to Validate: it drops entries
// that no longer resolve to a regular file (renamed or deleted since the
// instance was created) and returns a warning for each, instead of
// failing — a session-start loop would otherwise retry forever. Survivors
// are re-resolved through symlinks.
func Existing(paths []string) (kept, warnings []string) {
	for _, p := range paths {
		real, err := resolve(p)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		kept = append(kept, real)
	}
	return kept, warnings
}

// Encode renders paths for the registry: a single-line JSON array, or ""
// for none (so an instance without grants has an empty field).
func Encode(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	b, _ := json.Marshal(paths)
	return string(b)
}

// Decode is the inverse of Encode. Empty means no grants.
func Decode(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var paths []string
	if err := json.Unmarshal([]byte(s), &paths); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", RegistryKey, err)
	}
	return paths, nil
}
