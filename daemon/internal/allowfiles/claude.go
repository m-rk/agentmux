package allowfiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxSiblingRules bounds the deny rules ClaudeSettings will generate for
// the directory-grant fallback; a directory with more entries than this
// isn't fenced and the grant is dropped instead (see ClaudeSettings).
const maxSiblingRules = 5000

// claudeRulePath renders an absolute path as the body of a Claude Code
// permission rule. "//" marks an absolute filesystem path ("/" alone is
// relative to the settings file). Gitignore-style pattern characters are
// backslash-escaped; '(' and ')' also delimit the rule. Verified against
// Claude Code 2.1.288: spaces, \( \) \[ \] \* match only the literal
// file. '?' has no working escape, which is why Validate rejects it.
func claudeRulePath(abs string) string {
	var b strings.Builder
	b.WriteString("/")
	for _, r := range abs {
		if strings.ContainsRune(`[]()*`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

type claudePermissions struct {
	Allow                 []string `json:"allow"`
	Deny                  []string `json:"deny,omitempty"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
}

// ClaudeSettings renders the per-instance settings file passed to claude
// as --settings: Read and Edit allow rules for exactly the given files.
//
// Allow rules alone are exact, but they don't lift
// permissions.blockReadsOutsideWorkingDirectories: with that set, only a
// working or additional directory re-opens reads (verified; allow rules,
// a PreToolUse allow hook and pointing additionalDirectories at the file
// all fail). When blocked is true the file's parent directory is added
// as an additional directory and every other entry currently in it gets
// Read and Edit deny rules, so the grant stays the one file for
// everything that exists at launch. Entries created in that directory
// later are not fenced until the next session start. Files whose
// directory can't be fenced (unreadable, or over maxSiblingRules
// entries) are dropped from the result and reported in warnings.
func ClaudeSettings(files []string, blocked bool) (data []byte, warnings []string, err error) {
	perms := claudePermissions{Allow: []string{}}
	granted := map[string]bool{}
	for _, f := range files {
		granted[f] = true
	}
	fenced := map[string]bool{} // dir -> fenced successfully
	tried := map[string]bool{}
	for _, f := range files {
		if blocked {
			dir := filepath.Dir(f)
			if !tried[dir] {
				tried[dir] = true
				deny, derr := siblingDenies(dir, granted)
				if derr == nil {
					fenced[dir] = true
					perms.AdditionalDirectories = append(perms.AdditionalDirectories, dir)
					perms.Deny = append(perms.Deny, deny...)
				} else {
					warnings = append(warnings, fmt.Sprintf("allow-file %s: %v; not granted", dir, derr))
				}
			}
			if !fenced[dir] {
				continue
			}
		}
		p := claudeRulePath(f)
		perms.Allow = append(perms.Allow, "Read("+p+")", "Edit("+p+")")
	}
	data, err = json.MarshalIndent(map[string]any{"permissions": perms}, "", "  ")
	return append(data, '\n'), warnings, err
}

// siblingDenies returns Read and Edit deny rules for every entry of dir
// that isn't a granted file. Directories get a "/**" rule too.
func siblingDenies(dir string, granted map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxSiblingRules {
		return nil, fmt.Errorf("%s has %d entries, too many to fence", dir, len(entries))
	}
	var deny []string
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if granted[full] {
			continue
		}
		p := claudeRulePath(full)
		deny = append(deny, "Read("+p+")", "Edit("+p+")")
		if e.IsDir() {
			deny = append(deny, "Read("+p+"/**)", "Edit("+p+"/**)")
		}
	}
	return deny, nil
}

// ClaudeBlocksOutsideReads reports whether any of the given Claude Code
// settings files sets permissions.blockReadsOutsideWorkingDirectories to
// true. Missing or unparsable files count as not set. Managed (admin)
// settings aren't checked.
func ClaudeBlocksOutsideReads(settingsFiles ...string) bool {
	for _, p := range settingsFiles {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s struct {
			Permissions struct {
				Block bool `json:"blockReadsOutsideWorkingDirectories"`
			} `json:"permissions"`
		}
		if json.Unmarshal(data, &s) == nil && s.Permissions.Block {
			return true
		}
	}
	return false
}
