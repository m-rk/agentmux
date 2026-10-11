package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/runas"
	"gopkg.in/yaml.v3"
)

// defaultCodexAddDirs reads the optional host-wide writable-directory defaults.
func defaultCodexAddDirs() []string {
	home := runas.CurrentUserHome()
	data, err := os.ReadFile(filepath.Join(home, ".config", "agentmux", "codex.yaml"))
	if err != nil {
		return nil
	}
	var cfg struct {
		AddDirs []string `yaml:"add_dirs"`
	}
	if yaml.Unmarshal(data, &cfg) != nil {
		return nil
	}
	var out []string
	for _, d := range cfg.AddDirs {
		d = strings.TrimSpace(d)
		if filepath.IsAbs(d) && !strings.ContainsAny(d, ",\r\n") {
			out = append(out, d)
		}
	}
	return out
}

// EnsureCodexDefaults creates the host Codex add_dirs file when absent. The
// paths follow mergentic's local config resolution: local state defaults to
// ~/.agents/local and the vault comes from config.json (or MERGENTIC_VAULT).
func EnsureCodexDefaults(home string) ([]string, error) {
	if home == "" {
		home = runas.CurrentUserHome()
	}
	path := filepath.Join(home, ".config", "agentmux", "codex.yaml")
	if _, err := os.Stat(path); err == nil {
		return defaultCodexAddDirs(), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	localState := os.Getenv("MERGENTIC_LOCAL_STATE")
	if localState == "" {
		localState = filepath.Join(home, ".agents", "local")
	}
	var cfg struct {
		VaultPath string `json:"vault_path"`
	}
	data, err := os.ReadFile(filepath.Join(localState, "config.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, err
		}
	}
	vault := os.Getenv("MERGENTIC_VAULT")
	if vault == "" {
		vault = cfg.VaultPath
	}
	dirs := []string{localState}
	if vault != "" {
		dirs = append(dirs, filepath.Join(vault, "projects"))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return dirs, err
	}
	var body strings.Builder
	body.WriteString("add_dirs:\n")
	for _, dir := range dirs {
		fmt.Fprintf(&body, "  - %s\n", dir)
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		return dirs, err
	}
	return dirs, nil
}
