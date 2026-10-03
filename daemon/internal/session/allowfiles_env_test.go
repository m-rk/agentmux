package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/allowfiles"
)

func TestAllowFilesEnv(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "work")
	note := filepath.Join(root, "vault", "PP-1 note.md")
	for _, d := range []string{workdir, filepath.Dir(note)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(note, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal([]string{note})
	fields := map[string]string{allowfiles.RegistryKey: string(enc)}

	for agent, key := range map[string]string{"opencode": allowfiles.OpencodeConfigEnv, "kilo": allowfiles.KiloConfigEnv} {
		got := allowFilesEnv("t", agent, workdir, fields)
		if !strings.HasPrefix(got, key+"=") || !strings.Contains(got, "PP-1 note.md") {
			t.Errorf("%s: got %q", agent, got)
		}
	}
	for _, agent := range []string{"zero", "amp"} {
		if got := allowFilesEnv("t", agent, workdir, fields); got != "" {
			t.Errorf("%s: got %q, want none", agent, got)
		}
	}
	if got := allowFilesEnv("t", "opencode", workdir, map[string]string{}); got != "" {
		t.Errorf("no allow-files: got %q", got)
	}
	_ = os.Remove(note)
	if got := allowFilesEnv("t", "opencode", workdir, fields); got != "" {
		t.Errorf("missing note: got %q", got)
	}
}
