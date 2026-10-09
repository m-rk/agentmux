package ops

import (
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
