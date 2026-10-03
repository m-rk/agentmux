package allowfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// OpencodeConfigEnv is the environment variable opencode reads an inline JSON
// config from. It is merged after the project's opencode.json (and after the
// global config), so its permission rules are appended to the project's and,
// since the last matching rule wins, take precedence over them.
const OpencodeConfigEnv = "OPENCODE_CONFIG_CONTENT"

// OpencodeAllowConfig returns the inline JSON config that lets an opencode
// session started in workdir read and edit exactly the given files outside
// workdir, and nothing else under their directories, without a permission
// prompt. paths must be absolute and symlink-resolved, and so must workdir.
//
// The launcher applies it by setting OpencodeConfigEnv=<returned bytes> in the
// environment of every opencode process for the instance (tmux server
// included). Nothing is written to the worktree, so there is nothing to
// commit. Do not set it if the environment already carries its own
// OPENCODE_CONFIG_CONTENT; opencode takes only one.
//
// How it works (verified against opencode 1.18.34 with `opencode run`):
//   - Touching any path outside the workdir raises an external_directory
//     request whose pattern is "<parent dir>/*". There is no per-file form, so
//     the whole directory has to be allowed at that gate.
//   - The read and edit tools then match their own rules against the path
//     relative to the workdir ("../vault/x.md"), not the absolute path. A deny
//     on "<reldir>/*" followed by an allow on the exact relative file path
//     narrows the directory back down to the one file. Order matters (last
//     match wins), so all denies are emitted before all allows.
//
// Limits: bash, glob and grep are not path-gated once the directory passes
// external_directory, so an agent can still `cat` or `ls` siblings through
// bash. Reads and edits through the read/edit/write tools are confined.
// Files already inside workdir need no grant and are skipped.
//
// opencode's wildcard syntax has no escape for "*" and "?", and "{env:" /
// "{file:" are substituted in config text, so paths containing any of those
// are rejected rather than granted too broadly. "[", "]", "(", ")" and
// spaces match literally and are fine.
func OpencodeAllowConfig(workdir string, paths []string) ([]byte, error) {
	return opencodeStyleAllowConfig(workdir, paths)
}

var errAllowPathPattern = errors.New("path contains characters that opencode-style permission patterns cannot match literally")

// orderedRules is a JSON object whose keys keep insertion order. opencode and
// kilo resolve a permission by the last matching rule in document order, which
// a Go map would lose.
type orderedRules struct {
	keys   []string
	values []string
}

func (o *orderedRules) add(key, value string) {
	for _, k := range o.keys {
		if k == key {
			return
		}
	}
	o.keys = append(o.keys, key)
	o.values = append(o.values, value)
}

func (o orderedRules) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(o.values[i])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// opencodeStyleAllowConfig builds the permission block shared by opencode and
// kilo (kilo is an opencode fork with the same permission schema).
func opencodeStyleAllowConfig(workdir string, paths []string) ([]byte, error) {
	if !filepath.IsAbs(workdir) {
		return nil, fmt.Errorf("workdir %q is not absolute", workdir)
	}
	var external, denies, allows orderedRules
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("allowed file %q is not absolute", p)
		}
		if strings.ContainsAny(p, "*?") || strings.Contains(p, "{env:") || strings.Contains(p, "{file:") {
			return nil, fmt.Errorf("allowed file %q: %w", p, errAllowPathPattern)
		}
		rel, err := filepath.Rel(workdir, p)
		if err != nil {
			return nil, fmt.Errorf("allowed file %q: %w", p, err)
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // inside the workdir already
		}
		external.add(filepath.ToSlash(filepath.Dir(p))+"/*", "allow")
		denies.add(filepath.ToSlash(filepath.Dir(rel))+"/*", "deny")
		allows.add(filepath.ToSlash(rel), "allow")
	}
	if len(allows.keys) == 0 {
		return []byte("{}"), nil
	}
	// Denies first, then allows, into one ordered object per tool.
	var tool orderedRules
	for i, k := range denies.keys {
		tool.add(k, denies.values[i])
	}
	for i, k := range allows.keys {
		tool.add(k, allows.values[i])
	}
	doc := struct {
		Permission struct {
			ExternalDirectory orderedRules `json:"external_directory"`
			Read              orderedRules `json:"read"`
			Edit              orderedRules `json:"edit"`
		} `json:"permission"`
	}{}
	doc.Permission.ExternalDirectory = external
	doc.Permission.Read = tool
	doc.Permission.Edit = tool
	return json.Marshal(doc)
}
