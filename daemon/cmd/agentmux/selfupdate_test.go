package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSelfUpdatePlist(t *testing.T) {
	if !selfUpdatePlistSupported {
		t.Skip("plist rendering is darwin-only")
	}
	cfg := selfUpdateHostConfig{
		AgentmuxURL:  "https://example.com/agentmux.git",
		MergenticURL: "https://example.com/mergentic.git",
		AgentmuxDir:  "/home/me/src/agentmux",
		MergenticDir: "/home/me/src/mergentic",
	}
	_ = cfg
	plist := renderSelfUpdatePlistForTest(cfg)
	for _, want := range []string{
		"com.m-rk.agentmux.self-update",
		"<string>/home/me/.agentmux/bin/agentmux</string>",
		"<string>self-update</string>",
		"<string>run</string>",
		"<integer>600</integer>",
		"AGENTMUX_SELF_UPDATE_AGENTMUX_URL",
		"https://example.com/agentmux.git",
		"AGENTMUX_SELF_UPDATE_MERGENTIC_DIR",
		"/home/me/src/mergentic",
		"self-update.err.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	// No personal paths leak from the template itself: everything
	// host-specific arrives through cfg.
	if strings.Contains(renderSelfUpdatePlistForTest(selfUpdateHostConfig{}),
		"harley-mini") {
		t.Errorf("template contains a hostname")
	}
}

func TestParseShipCommits(t *testing.T) {
	sha := "2fe1a235a6f68f54bafcdcf2479e85c97b47a32e"
	got, err := parseShipCommits([]string{"agentmux@" + sha})
	if err != nil || got["agentmux"] != sha {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	for _, bad := range [][]string{
		{"agentmux@short"},
		{"bogus@" + sha},
		{"agentmux"},
		{"agentmux@" + sha + "@extra"},
	} {
		if _, err := parseShipCommits(bad); err == nil {
			t.Errorf("parseShipCommits(%q) succeeded", bad)
		}
	}
}

func TestInstallFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/new"
	dst := dir + "/bin/tool"
	if err := writeTestFile(src, "v2"); err != nil {
		t.Fatal(err)
	}
	if err := installFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(src, "v3"); err != nil {
		t.Fatal(err)
	}
	if err := installFile(src, dst); err != nil {
		t.Fatal(err)
	}
	// The second install kept v2 as the rollback binary.
	rollback, err := readTestFile(dst + ".prev")
	if err != nil || rollback != "v2" {
		t.Fatalf("rollback = %q, %v; want v2", rollback, err)
	}
	if err := rollbackFile(dst); err != nil {
		t.Fatal(err)
	}
	cur, err := readTestFile(dst)
	if err != nil || cur != "v2" {
		t.Fatalf("after rollback = %q, %v; want v2", cur, err)
	}
	if err := rollbackFile(dir + "/missing"); err == nil {
		t.Errorf("rollback without a .prev succeeded")
	}
}

func writeTestFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func readTestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func TestHomeOfFindsHome(t *testing.T) {
	if got := homeOf("/home/me/.agentmux/bin/agentmux"); got != "/home/me" {
		t.Errorf("homeOf pinned bin = %q, want /home/me", got)
	}
	if got := homeOf("/tmp/tool"); got == "" {
		t.Errorf("homeOf fallback is empty")
	}
}

func TestInstalledDoctorTime(t *testing.T) {
	dir := t.TempDir()
	home := dir + "/home"
	plistDir := home + "/Library/LaunchAgents"
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := selfUpdateInstalledDoctorTime(home); got != "" {
		t.Errorf("missing plist = %q, want empty", got)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>4</integer>
        <key>Minute</key>
        <integer>5</integer>
    </dict>
</dict>
</plist>
`
	if err := os.WriteFile(plistDir+"/com.m-rk.agentmux.doctor.plist", []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := selfUpdateInstalledDoctorTime(home); got != "04:05" {
		t.Errorf("doctor time = %q, want 04:05", got)
	}
}
