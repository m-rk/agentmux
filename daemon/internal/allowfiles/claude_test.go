package allowfiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestClaudeRulePath(t *testing.T) {
	cases := map[string]string{
		"/v/note.md":                     "//v/note.md",
		"/v/TASK-4 Some title.md":        "//v/TASK-4 Some title.md",
		"/v/a [x] (y) *z*.md":            `//v/a \[x\] \(y\) \*z\*.md`,
		"/Users/me/My Vault/t/TASK-1.md": "//Users/me/My Vault/t/TASK-1.md",
	}
	for in, want := range cases {
		if got := claudeRulePath(in); got != want {
			t.Errorf("claudeRulePath(%q) = %q, want %q", in, got, want)
		}
	}
}

type settingsDoc struct {
	Permissions struct {
		Allow                 []string `json:"allow"`
		Deny                  []string `json:"deny"`
		AdditionalDirectories []string `json:"additionalDirectories"`
	} `json:"permissions"`
}

func TestClaudeSettingsAllowOnly(t *testing.T) {
	data, warns, err := ClaudeSettings([]string{"/v/TASK-4 a [1].md"}, false)
	if err != nil || len(warns) != 0 {
		t.Fatal(err, warns)
	}
	var doc settingsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{`Read(//v/TASK-4 a \[1\].md)`, `Edit(//v/TASK-4 a \[1\].md)`}
	if !reflect.DeepEqual(doc.Permissions.Allow, want) {
		t.Errorf("allow = %q, want %q", doc.Permissions.Allow, want)
	}
	if len(doc.Permissions.Deny) != 0 || len(doc.Permissions.AdditionalDirectories) != 0 {
		t.Errorf("unblocked settings must be allow rules only, got %s", data)
	}
}

func TestClaudeSettingsBlockedFencesSiblings(t *testing.T) {
	dir := real(t, t.TempDir())
	note := write(t, filepath.Join(dir, "note.md"))
	write(t, filepath.Join(dir, "other.md"))
	write(t, filepath.Join(dir, "sub", "deep.md"))
	data, warns, err := ClaudeSettings([]string{note}, true)
	if err != nil || len(warns) != 0 {
		t.Fatal(err, warns)
	}
	var doc settingsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Permissions.AdditionalDirectories, []string{dir}) {
		t.Errorf("additionalDirectories = %q, want [%s]", doc.Permissions.AdditionalDirectories, dir)
	}
	wantAllow := []string{"Read(/" + dir + "/note.md)", "Edit(/" + dir + "/note.md)"}
	if !reflect.DeepEqual(doc.Permissions.Allow, wantAllow) {
		t.Errorf("allow = %q, want %q", doc.Permissions.Allow, wantAllow)
	}
	wantDeny := []string{
		"Read(/" + dir + "/other.md)", "Edit(/" + dir + "/other.md)",
		"Read(/" + dir + "/sub)", "Edit(/" + dir + "/sub)",
		"Read(/" + dir + "/sub/**)", "Edit(/" + dir + "/sub/**)",
	}
	if !reflect.DeepEqual(doc.Permissions.Deny, wantDeny) {
		t.Errorf("deny = %q, want %q", doc.Permissions.Deny, wantDeny)
	}
}

func TestClaudeSettingsBlockedUnfenceableDirDropsGrant(t *testing.T) {
	data, warns, err := ClaudeSettings([]string{"/nonexistent-dir-for-test/note.md"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var doc settingsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Permissions.Allow) != 0 || len(doc.Permissions.AdditionalDirectories) != 0 || len(warns) != 1 {
		t.Errorf("want no grant and one warning, got %s / %v", data, warns)
	}
}

func TestClaudeBlocksOutsideReads(t *testing.T) {
	dir := t.TempDir()
	on := filepath.Join(dir, "on.json")
	off := filepath.Join(dir, "off.json")
	os.WriteFile(on, []byte(`{"permissions":{"blockReadsOutsideWorkingDirectories":true}}`), 0o600)
	os.WriteFile(off, []byte(`{"permissions":{"defaultMode":"auto"}}`), 0o600)
	if !ClaudeBlocksOutsideReads(off, filepath.Join(dir, "missing.json"), on) {
		t.Error("want true when any file sets the block")
	}
	if ClaudeBlocksOutsideReads(off, filepath.Join(dir, "missing.json")) {
		t.Error("want false when no file sets the block")
	}
}
