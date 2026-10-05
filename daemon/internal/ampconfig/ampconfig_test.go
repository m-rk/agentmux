package ampconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "amp.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	cfg, err := Load(write(t, "mode: high\n"))
	if err != nil || cfg.Mode != "high" {
		t.Fatalf("load: %+v %v", cfg, err)
	}
	cfg, err = Load(write(t, "mode: \"  My Mode \"\n"))
	if err != nil || cfg.Mode != "My Mode" {
		t.Fatalf("trim: %+v %v", cfg, err)
	}
	if _, err := Load(write(t, "mode:\n")); err == nil {
		t.Fatal("empty mode should fail")
	}
	if _, err := Load(write(t, "mode: [high\n")); err == nil {
		t.Fatal("bad yaml should fail")
	}
	if cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err != nil || cfg.Mode != "" {
		t.Fatalf("missing: %+v %v", cfg, err)
	}
	if DefaultPath() == "" {
		t.Fatal("default path empty")
	}
}

func TestResolve(t *testing.T) {
	if m, s := Resolve(Config{Mode: "high"}, "custom"); m != "custom" || s != "instance" {
		t.Fatalf("instance override: %q %q", m, s)
	}
	if m, s := Resolve(Config{Mode: "high"}, ""); m != "high" || s != "host" {
		t.Fatalf("host: %q %q", m, s)
	}
	if m, s := Resolve(Config{}, ""); m != "" || s != "" {
		t.Fatalf("unset: %q %q", m, s)
	}
}
