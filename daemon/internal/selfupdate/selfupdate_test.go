package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBasePathsStayUnderAgentmuxDir(t *testing.T) {
	home := "/home/alice"
	for _, p := range []string{BaseDir(home), ReposDir(home), ShippedPath(home), VersionsPath(home), LogPath(home), LockPath(home)} {
		if filepath.Dir(p) != BaseDir(home) && p != BaseDir(home) && filepath.Dir(filepath.Dir(p)) != BaseDir(home) {
			// Repos live one deeper; everything else is directly inside.
			if filepath.Dir(p) != ReposDir(home) {
				t.Errorf("path %q escapes the state dir", p)
			}
		}
	}
	if got := BaseDir(home); got != "/home/alice/.agentmux/self-update" {
		t.Errorf("BaseDir = %q", got)
	}
}

func TestValidSHA(t *testing.T) {
	full := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	if !ValidSHA(full) {
		t.Errorf("full sha rejected")
	}
	for _, bad := range []string{"", "2fe1a23", "2FE1A235A6F68F54BAFCDCF2479E85C97B47A32E", full + "e", "xyz"} {
		if ValidSHA(bad) {
			t.Errorf("ValidSHA(%q) = true", bad)
		}
	}
}

func TestShippedGateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shipped.json")
	got, err := LoadCommits(path)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file = %+v, %v; want empty", got, err)
	}
	sha := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	if err := SaveCommits(path, map[string]string{"agentmux": sha, "bogus": sha, "mergentic": "short"}); err != nil {
		t.Fatal(err)
	}
	got, err = LoadCommits(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["agentmux"] != sha || len(got) != 1 {
		t.Errorf("reloaded = %+v; want only agentmux", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind")
	}
}

func TestWantInstall(t *testing.T) {
	shaA := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	shaB := "b3e7b46abcdef1234567890abcdef1234567890a"
	cases := []struct {
		name      string
		repo      string
		installed string
		shipped   string
		want      bool
	}{
		{"moves when gate is ahead", "agentmux", shaA, shaB, true},
		{"stays when already there", "agentmux", shaA, shaA, false},
		{"stays when no gate", "agentmux", shaA, "", false},
		{"stays on short sha", "agentmux", shaA, "b3e7b46", false},
		{"stays on unknown repo", "bogus", "", shaA, false},
		{"fresh install when gate set", "mergentic", "", shaA, true},
	}
	for _, c := range cases {
		if got := WantInstall(c.repo, c.installed, c.shipped); got != c.want {
			t.Errorf("%s: WantInstall = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLocked(t *testing.T) {
	home := t.TempDir()
	if Locked(home) {
		t.Errorf("no lock file means unlocked")
	}
	if err := os.MkdirAll(BaseDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LockPath(home), []byte("deploying\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Locked(home) {
		t.Errorf("fresh lock means locked")
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(LockPath(home), stale, stale); err != nil {
		t.Fatal(err)
	}
	if Locked(home) {
		t.Errorf("stale lock must not block the updater")
	}
}

func TestLogLines(t *testing.T) {
	home := t.TempDir()
	sha := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	if err := LogEvent(home, DeployedLine("agentmux", "", sha)); err != nil {
		t.Fatal(err)
	}
	if err := LogEvent(home, FailedLine("mergentic", sha, "go vet failed")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(LogPath(home))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{"deployed agentmux@" + sha + " old=none", "failed mergentic@" + sha + ": go vet failed"} {
		found := false
		for _, line := range splitLines(content) {
			if len(line) > 20 && contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("log missing %q:\n%s", want, content)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
