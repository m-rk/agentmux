package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderSelfUpdatePlist(t *testing.T) {
	cfg := selfUpdateHostConfig{
		AgentmuxURL:  "https://example.com/agentmux.git",
		MergenticURL: "https://example.com/mergentic.git",
		AgentmuxDir:  "/home/me/src/agentmux",
		MergenticDir: "/home/me/src/mergentic",
		AgentmuxBin:  "/home/me/.agentmux/bin/agentmux",
		MergenticBin: "/home/me/.local/bin/mergentic",
		AgentsBinDir: "/home/me/.local/bin",
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
		"AGENTMUX_SELF_UPDATE_MERGENTIC_URL",
		"https://example.com/mergentic.git",
		"AGENTMUX_SELF_UPDATE_AGENTMUX_DIR",
		"/home/me/src/agentmux",
		"AGENTMUX_SELF_UPDATE_MERGENTIC_DIR",
		"/home/me/src/mergentic",
		"AGENTMUX_SELF_UPDATE_AGENTMUX_BIN",
		"AGENTMUX_SELF_UPDATE_MERGENTIC_BIN",
		"AGENTMUX_SELF_UPDATE_AGENTS_BIN_DIR",
		"/home/me/.local/bin",
		"AGENTMUX_SELF_UPDATE_SOCKET",
		"AGENTMUX_SELF_UPDATE_GO_BIN",
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

func TestResolveSelfUpdateGoWithEmptyPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf go-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSelfUpdateGo(bin, "")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("resolved Go path %q is not absolute", got)
	}
	cmd := exec.Command(got, "version")
	cmd.Env = []string{"PATH="}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "go-ok" {
		t.Fatalf("Go with empty PATH = %q, %v", out, err)
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

// selfUpdateFlagEnvCases covers every host-config field: flag value,
// environment value, and the empty case each parse path must resolve.
func selfUpdateFlagEnvCases() []struct {
	name  string
	flag  string
	value string
	env   string
	envV  string
	get   func(selfUpdateHostConfig) string
} {
	return []struct {
		name  string
		flag  string
		value string
		env   string
		envV  string
		get   func(selfUpdateHostConfig) string
	}{
		{"agentmux-url", "-agentmux-url", "https://example.com/a-flag.git", "AGENTMUX_SELF_UPDATE_AGENTMUX_URL", "https://example.com/a-env.git", func(c selfUpdateHostConfig) string { return c.AgentmuxURL }},
		{"mergentic-url", "-mergentic-url", "https://example.com/m-flag.git", "AGENTMUX_SELF_UPDATE_MERGENTIC_URL", "https://example.com/m-env.git", func(c selfUpdateHostConfig) string { return c.MergenticURL }},
		{"agentmux-dir", "-agentmux-dir", "/flag/agentmux", "AGENTMUX_SELF_UPDATE_AGENTMUX_DIR", "/env/agentmux", func(c selfUpdateHostConfig) string { return c.AgentmuxDir }},
		{"mergentic-dir", "-mergentic-dir", "/flag/mergentic", "AGENTMUX_SELF_UPDATE_MERGENTIC_DIR", "/env/mergentic", func(c selfUpdateHostConfig) string { return c.MergenticDir }},
		{"agentmux-bin", "-agentmux-bin", "/flag/agentmux-bin", "AGENTMUX_SELF_UPDATE_AGENTMUX_BIN", "/env/agentmux-bin", func(c selfUpdateHostConfig) string { return c.AgentmuxBin }},
		{"mergentic-bin", "-mergentic-bin", "/flag/mergentic-bin", "AGENTMUX_SELF_UPDATE_MERGENTIC_BIN", "/env/mergentic-bin", func(c selfUpdateHostConfig) string { return c.MergenticBin }},
		{"agents-bin-dir", "-agents-bin-dir", "/flag/agents-bin", "AGENTMUX_SELF_UPDATE_AGENTS_BIN_DIR", "/env/agents-bin", func(c selfUpdateHostConfig) string { return c.AgentsBinDir }},
		{"socket", "-socket", "/flag/agentmuxd.sock", "AGENTMUX_SELF_UPDATE_SOCKET", "/env/agentmuxd.sock", func(c selfUpdateHostConfig) string { return c.GatewaySocket }},
	}
}

func withSelfUpdateEnv(t *testing.T, k, v string) {
	t.Helper()
	if v == "" {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv(k, v)
}

func clearSelfUpdateEnv(t *testing.T) {
	t.Helper()
	for _, c := range selfUpdateFlagEnvCases() {
		withSelfUpdateEnv(t, c.env, "")
	}
}

// TestSelfUpdateInstallFlagEnvDefault checks flag > env > default for
// `self-update install` on every field: a passed flag wins over the
// environment, the environment wins when no flag is passed, and the
// fields with home-based defaults fall back to them.
func TestSelfUpdateInstallFlagEnvDefault(t *testing.T) {
	for _, c := range selfUpdateFlagEnvCases() {
		// Flag beats env.
		clearSelfUpdateEnv(t)
		withSelfUpdateEnv(t, c.env, c.envV)
		_, _, cfg, err := parseSelfUpdateInstallArgs([]string{c.flag, c.value})
		if err != nil {
			t.Fatalf("%s: flag parse: %v", c.name, err)
		}
		if got := c.get(cfg); got != c.value {
			t.Errorf("%s: flag over env = %q, want %q", c.name, got, c.value)
		}
		// Env fills in when no flag is passed.
		clearSelfUpdateEnv(t)
		withSelfUpdateEnv(t, c.env, c.envV)
		_, _, cfg, err = parseSelfUpdateInstallArgs(nil)
		if err != nil {
			t.Fatalf("%s: env parse: %v", c.name, err)
		}
		if got := c.get(cfg); got != c.envV {
			t.Errorf("%s: env = %q, want %q", c.name, got, c.envV)
		}
	}
	// Empty env and no flags: dirs and bins fall back to home-based
	// defaults, URLs and the agents bin dir stay empty.
	clearSelfUpdateEnv(t)
	_, _, cfg, err := parseSelfUpdateInstallArgs(nil)
	if err != nil {
		t.Fatalf("defaults parse: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := selfUpdateHostConfig{
		AgentmuxDir:  filepath.Join(home, "src", "agentmux"),
		MergenticDir: filepath.Join(home, "src", "mergentic"),
		AgentmuxBin:  filepath.Join(home, ".agentmux", "bin", "agentmux"),
		MergenticBin: filepath.Join(home, ".local", "bin", "mergentic"),
	}
	got := selfUpdateHostConfig{
		AgentmuxDir:  cfg.AgentmuxDir,
		MergenticDir: cfg.MergenticDir,
		AgentmuxBin:  cfg.AgentmuxBin,
		MergenticBin: cfg.MergenticBin,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	if cfg.AgentmuxURL != "" || cfg.MergenticURL != "" || cfg.AgentsBinDir != "" {
		t.Errorf("URL/agents-bin defaults = %+v, want empty", cfg)
	}
	if cfg.GatewaySocket == "" {
		t.Errorf("socket default is empty")
	}
}

// TestSelfUpdateRunFlagEnvDefault checks flag > env on every field for
// `self-update run`, the same precedence `install` bakes into the plist.
func TestSelfUpdateRunFlagEnvDefault(t *testing.T) {
	for _, c := range selfUpdateFlagEnvCases() {
		clearSelfUpdateEnv(t)
		withSelfUpdateEnv(t, c.env, c.envV)
		cfg, err := parseSelfUpdateRunArgs([]string{c.flag, c.value})
		if err != nil {
			t.Fatalf("%s: flag parse: %v", c.name, err)
		}
		if got := c.get(cfg); got != c.value {
			t.Errorf("%s: flag over env = %q, want %q", c.name, got, c.value)
		}
		clearSelfUpdateEnv(t)
		withSelfUpdateEnv(t, c.env, c.envV)
		cfg, err = parseSelfUpdateRunArgs(nil)
		if err != nil {
			t.Fatalf("%s: env parse: %v", c.name, err)
		}
		if got := c.get(cfg); got != c.envV {
			t.Errorf("%s: env = %q, want %q", c.name, got, c.envV)
		}
	}
}

// TestSelfUpdateInstallFlagsReachPlist replays the reported repro: the
// docs command's flags must land in the printed plist, including the
// agents bin dir run uses for the extra mergentic copy.
func TestSelfUpdateInstallFlagsReachPlist(t *testing.T) {
	clearSelfUpdateEnv(t)
	_, print, cfg, err := parseSelfUpdateInstallArgs([]string{
		"-print",
		"-agentmux-url", "https://example.com/agentmux.git",
		"-mergentic-url", "https://example.com/mergentic.git",
		"-agentmux-dir", "/home/me/src/agentmux",
		"-mergentic-dir", "/home/me/src/mergentic",
		"-agents-bin-dir", "/home/me/.local/bin",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !print {
		t.Errorf("-print did not parse")
	}
	if cfg.AgentmuxURL == "" || cfg.MergenticURL == "" {
		t.Fatalf("URLs did not survive parsing: %+v", cfg)
	}
	plist := renderSelfUpdatePlistForTest(cfg)
	for _, want := range []string{
		"https://example.com/agentmux.git",
		"https://example.com/mergentic.git",
		"/home/me/src/agentmux",
		"/home/me/src/mergentic",
		"AGENTMUX_SELF_UPDATE_AGENTS_BIN_DIR",
		"/home/me/.local/bin",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
}
